package artifactgrpc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/artifactgrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

const (
	workerName = "elitea-worker"
	otherName  = "elitea-intruder"
	token7     = "token-of-project-7"
)

// fakeArtifacts stands in for elitea-main's artifact routes: the same paths
// and wire shapes, a bearer that belongs to project 7 only, and objects in
// memory. The adapter under test must reach it exactly as an HTTP client did.
type fakeArtifacts struct {
	mu       sync.Mutex
	objects  map[string]stored // "{project}/{bucket}/{key}"
	buckets  map[string]bool
	claims   []http.Header
	started  chan struct{}
	canceled chan struct{}
	aborted  chan struct{}
}

type stored struct {
	data      []byte
	mediaType string
}

func newFake() *fakeArtifacts {
	return &fakeArtifacts{
		objects:  map[string]stored{},
		buckets:  map[string]bool{"7/docs": true},
		started:  make(chan struct{}, 1),
		canceled: make(chan struct{}, 1),
		aborted:  make(chan struct{}, 1),
	}
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": kind, "message": message}})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func (f *fakeArtifacts) handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token7 {
				writeError(w, http.StatusUnauthorized, "Unauthenticated", "invalid token")
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	project := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if chi.URLParam(r, "projectID") != "7" {
				writeError(w, http.StatusForbidden, "Forbidden", "no access to this project")
				return
			}
			if r.Header.Get("X-Elitea-Claim-Id") != "" {
				f.mu.Lock()
				f.claims = append(f.claims, r.Header.Clone())
				f.mu.Unlock()
			}
			next(w, r)
		}
	}
	r.Route("/api/v2/artifacts", func(r chi.Router) {
		r.Get("/buckets/{projectID}", project(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			buckets := []map[string]any{}
			for name := range f.buckets {
				buckets = append(buckets, map[string]any{"name": strings.TrimPrefix(name, "7/"), "retention_days": nil, "size_bytes": 0, "object_count": 0})
			}
			writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
		}))
		r.Post("/buckets/{projectID}", project(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name          string `json:"name"`
				RetentionDays *int32 `json:"retention_days"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.buckets["7/"+body.Name] = true
			f.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"name": body.Name, "retention_days": body.RetentionDays, "size_bytes": 0, "object_count": 0})
		}))
		r.Get("/objects/{projectID}/{bucket}", project(f.list))
		r.Post("/objects/{projectID}/{bucket}", project(f.upload))
		r.Post("/objects/{projectID}/{bucket}:batchDelete", project(f.batchDelete))
		r.Get("/objects/{projectID}/{bucket}/*", project(f.download))
		r.Head("/objects/{projectID}/{bucket}/*", project(f.stat))
		r.Delete("/objects/{projectID}/{bucket}/*", project(f.remove))
	})
	return r
}

func objectName(r *http.Request) string {
	return chi.URLParam(r, "projectID") + "/" + chi.URLParam(r, "bucket") + "/" + chi.URLParam(r, "*")
}

func (f *fakeArtifacts) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := "7/" + chi.URLParam(r, "bucket") + "/" + r.URL.Query().Get("prefix")
	var keys []string
	for name := range f.objects {
		if strings.HasPrefix(name, prefix) {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 1000
	}
	end := min(start+limit, len(keys))
	objects := []map[string]any{}
	for _, name := range keys[start:end] {
		object := f.objects[name]
		objects = append(objects, map[string]any{
			"key": strings.SplitN(name, "/", 3)[2], "size_bytes": len(object.data), "media_type": object.mediaType,
			"etag": fmt.Sprintf("\"%d\"", len(object.data)), "modified_at": "2026-10-21T10:00:00Z",
		})
	}
	answer := map[string]any{"objects": objects, "common_prefixes": []string{}}
	if end < len(keys) {
		answer["next_cursor"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, answer)
}

func (f *fakeArtifacts) upload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "not multipart")
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "file field is required")
		return
	}
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || params["filename"] == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "no filename")
		return
	}
	data, err := io.ReadAll(part)
	if err != nil {
		f.aborted <- struct{}{}
		writeError(w, http.StatusBadRequest, "InvalidArgument", "the body broke")
		return
	}
	name := chi.URLParam(r, "projectID") + "/" + chi.URLParam(r, "bucket") + "/" + params["filename"]
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[name]; exists && r.URL.Query().Get("overwrite") != "true" {
		writeError(w, http.StatusConflict, "AlreadyExists", "key already exists")
		return
	}
	mediaType := part.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "text/markdown"
	}
	f.objects[name] = stored{data: data, mediaType: mediaType}
	writeJSON(w, http.StatusCreated, map[string]any{"key": params["filename"], "size_bytes": len(data), "media_type": mediaType, "etag": "\"e\""})
}

func (f *fakeArtifacts) lookup(w http.ResponseWriter, r *http.Request) (stored, bool) {
	f.mu.Lock()
	object, ok := f.objects[objectName(r)]
	f.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "NotFound", "no such key")
	}
	return object, ok
}

func (f *fakeArtifacts) download(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "*") == "hang.bin" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 10))
		w.(http.Flusher).Flush()
		f.started <- struct{}{}
		<-r.Context().Done()
		f.canceled <- struct{}{}
		return
	}
	object, ok := f.lookup(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", object.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(object.data)))
	w.Header().Set("ETag", "\"e\"")
	w.Header().Set("Last-Modified", time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	// Odd-sized writes, so chunking is the adapter's, not the handler's.
	for data := object.data; len(data) > 0; {
		n := min(333_333, len(data))
		_, _ = w.Write(data[:n])
		data = data[n:]
	}
}

func (f *fakeArtifacts) stat(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	object, ok := f.objects[objectName(r)]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", object.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(object.data)))
	w.Header().Set("ETag", "\"e\"")
	w.Header().Set("Last-Modified", time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeArtifacts) remove(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.lookup(w, r); !ok {
		return
	}
	f.mu.Lock()
	delete(f.objects, objectName(r))
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeArtifacts) batchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	deleted, failed := []string{}, []map[string]string{}
	f.mu.Lock()
	for _, key := range body.Keys {
		name := "7/" + chi.URLParam(r, "bucket") + "/" + key
		if _, ok := f.objects[name]; ok {
			delete(f.objects, name)
			deleted = append(deleted, key)
		} else {
			failed = append(failed, map[string]string{"key": key, "code": "NotFound", "message": "no such key"})
		}
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "failed": failed})
}

func start(t *testing.T, pki *grpcauthtest.PKI, handler http.Handler) string {
	t.Helper()
	service, err := artifactgrpc.NewServer(handler, nil)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := grpcauth.NewAllowlist([]string{"dns:" + workerName})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: listener.Addr().String(),
		TLS:     pki.ServerTLS(t),
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	}, service.PlatformService(clients))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return listener.Addr().String()
}

func client(t *testing.T, address string, tlsConfig *tls.Config) runtimev1.ArtifactObjectServiceClient {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(runtimegrpc.PlatformMaxMessageBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return runtimev1.NewArtifactObjectServiceClient(connection)
}

func bearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func put(ctx context.Context, artifacts runtimev1.ArtifactObjectServiceClient, head *runtimev1.ArtifactPutObjectHeadV1, data []byte, chunk int) (*runtimev1.ArtifactPutObjectResponseV1, error) {
	stream, err := artifacts.PutObject(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Head{Head: head}}); err != nil {
		return nil, err
	}
	for len(data) > 0 {
		n := min(chunk, len(data))
		if err := stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Data{Data: data[:n]}}); err != nil {
			return nil, err
		}
		data = data[n:]
	}
	return stream.CloseAndRecv()
}

func get(ctx context.Context, artifacts runtimev1.ArtifactObjectServiceClient, ref *runtimev1.ArtifactObjectRefV1) (*runtimev1.ArtifactObjectInfoV1, []byte, int, error) {
	stream, err := artifacts.GetObject(ctx, &runtimev1.ArtifactGetObjectRequestV1{Object: ref})
	if err != nil {
		return nil, nil, 0, err
	}
	var info *runtimev1.ArtifactObjectInfoV1
	var body bytes.Buffer
	chunks := 0
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return info, body.Bytes(), chunks, nil
		}
		if err != nil {
			return info, body.Bytes(), chunks, err
		}
		switch f := frame.GetFrame().(type) {
		case *runtimev1.ArtifactGetObjectResponseV1_Info:
			info = f.Info
		case *runtimev1.ArtifactGetObjectResponseV1_Data:
			if len(f.Data) > artifactgrpc.ChunkBytes {
				return nil, nil, 0, fmt.Errorf("a chunk of %d bytes", len(f.Data))
			}
			chunks++
			body.Write(f.Data)
		}
	}
}

func refusalOf(t *testing.T, trailer metadata.MD) *runtimev1.ArtifactRefusalV1 {
	t.Helper()
	values := trailer.Get(artifactgrpc.RefusalTrailer)
	if len(values) != 1 {
		t.Fatalf("refusal trailer = %v", values)
	}
	refusal := &runtimev1.ArtifactRefusalV1{}
	if err := proto.Unmarshal([]byte(values[0]), refusal); err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestAuthorizedCallerRoundTripsAMultiChunkObject(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	artifacts := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))
	ctx := bearer(context.Background(), token7)

	created, err := artifacts.CreateBucket(ctx, &runtimev1.ArtifactCreateBucketRequestV1{ProjectId: 7, Name: "decks", RetentionDays: proto.Int32(30)})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetBucket().GetName() != "decks" || created.GetBucket().GetRetentionDays() != 30 {
		t.Errorf("created %v", created)
	}
	buckets, err := artifacts.ListBuckets(ctx, &runtimev1.ArtifactListBucketsRequestV1{ProjectId: 7})
	if err != nil || len(buckets.GetBuckets()) != 2 {
		t.Fatalf("buckets %v, %v", buckets, err)
	}

	// 3.5 MiB of random bytes, sent in 700 KiB frames and answered in odd
	// writes: the object crosses several chunks both ways.
	data := make([]byte, 3*1024*1024+512*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	key := "reports/Q3 résumé.pptx"
	claim := &runtimev1.ArtifactRuntimeClaimV1{ExecutionId: "exec-1", Generation: 3, ClaimId: "claim-1", Fence: "fence-1"}
	written, err := put(ctx, artifacts, &runtimev1.ArtifactPutObjectHeadV1{
		ProjectId: 7, Bucket: "decks", Key: key, Overwrite: true, Claim: claim,
		MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	}, data, 700*1024)
	if err != nil {
		t.Fatal(err)
	}
	if written.GetInfo().GetKey() != key || written.GetInfo().GetSizeBytes() != int64(len(data)) {
		t.Errorf("written %v", written.GetInfo())
	}
	ref := &runtimev1.ArtifactObjectRefV1{ProjectId: 7, Bucket: "decks", Key: key, Claim: claim}
	info, body, chunks, err := get(ctx, artifacts, ref)
	if err != nil {
		t.Fatal(err)
	}
	// The info frame carries the version a reader pins: etag and modified_at.
	if info.GetEtag() != "\"e\"" || info.GetModifiedAt() != "2026-10-21T10:00:00Z" {
		t.Errorf("get info %v", info)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("the object came back different: %d bytes, want %d", len(body), len(data))
	}
	if chunks < 4 || info.GetSizeBytes() != int64(len(data)) || !strings.HasPrefix(info.GetMediaType(), "application/vnd.openxml") {
		t.Errorf("chunks %d, info %v", chunks, info)
	}
	stat, err := artifacts.Stat(ctx, &runtimev1.ArtifactStatRequestV1{Object: ref})
	if err != nil {
		t.Fatal(err)
	}
	if stat.GetInfo().GetSizeBytes() != int64(len(data)) || stat.GetInfo().GetModifiedAt() != "2026-10-21T10:00:00Z" || stat.GetInfo().GetEtag() != "\"e\"" {
		t.Errorf("stat %v", stat.GetInfo())
	}

	// Without overwrite, a taken key is the route's 409.
	_, err = put(ctx, artifacts, &runtimev1.ArtifactPutObjectHeadV1{ProjectId: 7, Bucket: "decks", Key: key}, []byte("x"), 1)
	if status.Code(err) != codes.Aborted {
		t.Errorf("a second write without overwrite: %v", err)
	}

	fake.mu.Lock()
	claims := len(fake.claims)
	header := fake.claims[0]
	fake.mu.Unlock()
	if claims < 3 || header.Get("X-Elitea-Execution-Generation") != "3" || header.Get("X-Elitea-Fence") != "fence-1" {
		t.Errorf("the claim reached the route %d times as %v", claims, header)
	}

	if _, err := artifacts.Delete(ctx, &runtimev1.ArtifactDeleteRequestV1{Object: ref}); err != nil {
		t.Fatal(err)
	}
	var trailer metadata.MD
	_, err = artifacts.Stat(ctx, &runtimev1.ArtifactStatRequestV1{Object: ref}, grpc.Trailer(&trailer))
	if status.Code(err) != codes.NotFound || refusalOf(t, trailer).GetHttpStatus() != http.StatusNotFound {
		t.Errorf("stat after delete: %v", err)
	}
}

func TestListObjectsFollowsTheCursorAndBatchDeletes(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	artifacts := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))
	ctx := bearer(context.Background(), token7)
	for i := range 5 {
		if _, err := put(ctx, artifacts, &runtimev1.ArtifactPutObjectHeadV1{ProjectId: 7, Bucket: "docs", Key: fmt.Sprintf("wiki/p%d.md", i)}, []byte("page"), 4); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := artifacts.ListObjects(ctx, &runtimev1.ArtifactListObjectsRequestV1{ProjectId: 7, Bucket: "docs", Prefix: "wiki/", PageSize: 2, MaxPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	pages := 0
	for {
		page, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, object := range page.GetObjects() {
			if object.GetMediaType() != "text/markdown" || object.GetModifiedAt() == "" {
				t.Errorf("object %v", object)
			}
			keys = append(keys, object.GetKey())
		}
	}
	if pages != 3 || len(keys) != 5 {
		t.Fatalf("%d pages, keys %v", pages, keys)
	}

	// One page only: the caller can resume from its cursor.
	one, err := artifacts.ListObjects(ctx, &runtimev1.ArtifactListObjectsRequestV1{ProjectId: 7, Bucket: "docs", PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := one.Recv()
	if err != nil || first.GetNextCursor() != "2" {
		t.Fatalf("first page %v, %v", first, err)
	}
	if _, err := one.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("a one-page listing went on: %v", err)
	}

	batch, err := artifacts.DeleteBatch(ctx, &runtimev1.ArtifactDeleteBatchRequestV1{ProjectId: 7, Bucket: "docs", Keys: []string{"wiki/p0.md", "wiki/missing.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.GetDeleted()) != 1 || len(batch.GetFailed()) != 1 || batch.GetFailed()[0].GetCode() != "NotFound" {
		t.Errorf("batch %v", batch)
	}
}

func TestRefusals(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	address := start(t, pki, fake.handler())
	request := &runtimev1.ArtifactListBucketsRequestV1{ProjectId: 7}

	t.Run("wrong identity", func(t *testing.T) {
		artifacts := client(t, address, pki.ClientTLS(t, otherName))
		_, err := artifacts.ListBuckets(bearer(context.Background(), token7), request)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("an unlisted workload: %v", err)
		}
	})
	t.Run("no certificate", func(t *testing.T) {
		artifacts := client(t, address, pki.AnonymousTLS())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := artifacts.ListBuckets(bearer(ctx, token7), request)
		if code := status.Code(err); code != codes.Unavailable {
			t.Fatalf("no client certificate: %v", err)
		}
	})
	t.Run("bad token", func(t *testing.T) {
		artifacts := client(t, address, pki.ClientTLS(t, workerName))
		var trailer metadata.MD
		_, err := artifacts.ListBuckets(bearer(context.Background(), "forged"), request, grpc.Trailer(&trailer))
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("a bad bearer: %v", err)
		}
		if refusal := refusalOf(t, trailer); refusal.GetHttpStatus() != http.StatusUnauthorized || refusal.GetCode() != "Unauthenticated" {
			t.Errorf("refusal %v", refusal)
		}
	})
	t.Run("no token", func(t *testing.T) {
		artifacts := client(t, address, pki.ClientTLS(t, workerName))
		if _, err := artifacts.ListBuckets(context.Background(), request); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("no bearer: %v", err)
		}
	})
	t.Run("cross-project object", func(t *testing.T) {
		artifacts := client(t, address, pki.ClientTLS(t, workerName))
		var trailer metadata.MD
		_, _, _, err := get(bearer(context.Background(), token7), artifacts,
			&runtimev1.ArtifactObjectRefV1{ProjectId: 8, Bucket: "docs", Key: "secret.md"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("project 7's bearer read project 8: %v", err)
		}
		_ = trailer
		_, err = artifacts.Stat(bearer(context.Background(), token7),
			&runtimev1.ArtifactStatRequestV1{Object: &runtimev1.ArtifactObjectRefV1{ProjectId: 8, Bucket: "docs", Key: "secret.md"}}, grpc.Trailer(&trailer))
		if status.Code(err) != codes.PermissionDenied || refusalOf(t, trailer).GetHttpStatus() != http.StatusForbidden {
			t.Fatalf("project 7's bearer stat project 8: %v", err)
		}
	})
	t.Run("a key that would change the route", func(t *testing.T) {
		artifacts := client(t, address, pki.ClientTLS(t, workerName))
		for _, key := range []string{"../8/docs/x", "a//b", "a\\b", ""} {
			_, err := artifacts.Stat(bearer(context.Background(), token7),
				&runtimev1.ArtifactStatRequestV1{Object: &runtimev1.ArtifactObjectRefV1{ProjectId: 7, Bucket: "docs", Key: key}})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("key %q: %v", key, err)
			}
		}
	})
}

func TestCancellation(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	artifacts := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))

	t.Run("get", func(t *testing.T) {
		ctx, cancel := context.WithCancel(bearer(context.Background(), token7))
		defer cancel()
		stream, err := artifacts.GetObject(ctx, &runtimev1.ArtifactGetObjectRequestV1{Object: &runtimev1.ArtifactObjectRefV1{ProjectId: 7, Bucket: "docs", Key: "hang.bin"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		<-fake.started
		cancel()
		select {
		case <-fake.canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelling the call did not cancel the handler")
		}
	})
	t.Run("put", func(t *testing.T) {
		ctx, cancel := context.WithCancel(bearer(context.Background(), token7))
		stream, err := artifacts.PutObject(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Head{Head: &runtimev1.ArtifactPutObjectHeadV1{ProjectId: 7, Bucket: "docs", Key: "half.bin"}}})
		_ = stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Data{Data: []byte("half of it")}})
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-fake.aborted:
		case <-time.After(5 * time.Second):
			t.Fatal("a cancelled upload did not break the handler's body")
		}
		fake.mu.Lock()
		_, kept := fake.objects["7/docs/half.bin"]
		fake.mu.Unlock()
		if kept {
			t.Fatal("a cancelled upload stored an object")
		}
	})
}

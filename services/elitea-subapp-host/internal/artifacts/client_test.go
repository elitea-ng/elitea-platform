package artifacts_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// fakeService stands in for elitea-main's ArtifactObjectService: bearer
// "token" may use project 7 only, objects live in memory, and every refusal
// travels as the real one does — a gRPC status plus the refusal trailer.
type fakeService struct {
	runtimev1.UnimplementedArtifactObjectServiceServer
	mu       sync.Mutex
	objects  map[string][]byte
	types    map[string]string
	putHeads []*runtimev1.ArtifactPutObjectHeadV1
	frames   int
	pages    int
	// hang blocks GetObject after the info frame until the call ends.
	hangKey string
	hung    chan struct{}
}

func newFakeService() *fakeService {
	return &fakeService{objects: map[string][]byte{}, types: map[string]string{}, hung: make(chan struct{}, 1)}
}

func refuse(ctx context.Context, code codes.Code, httpStatus int32, errCode, message string) error {
	encoded, _ := proto.Marshal(&runtimev1.ArtifactRefusalV1{HttpStatus: httpStatus, Code: errCode, Message: message})
	_ = grpc.SetTrailer(ctx, metadata.Pairs(artifacts.RefusalTrailer, string(encoded)))
	return status.Error(code, message)
}

func (f *fakeService) authorize(ctx context.Context, project int64) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer token" {
		return refuse(ctx, codes.Unauthenticated, 401, "unauthenticated", "bad bearer")
	}
	if project != 7 {
		return refuse(ctx, codes.PermissionDenied, 403, "forbidden", "no access to project")
	}
	return nil
}

func objectID(project int64, bucket, key string) string {
	return strings.Join([]string{string(rune('0' + project)), bucket, key}, "/")
}

func (f *fakeService) PutObject(stream grpc.ClientStreamingServer[runtimev1.ArtifactPutObjectRequestV1, runtimev1.ArtifactPutObjectResponseV1]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	head := first.GetHead()
	if err := f.authorize(stream.Context(), head.GetProjectId()); err != nil {
		return err
	}
	var data bytes.Buffer
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.frames++
		f.mu.Unlock()
		data.Write(frame.GetData())
	}
	if head.GetBucket() == "full" {
		return refuse(stream.Context(), codes.ResourceExhausted, 507, "quota", "bucket is full")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putHeads = append(f.putHeads, head)
	id := objectID(head.GetProjectId(), head.GetBucket(), head.GetKey())
	f.objects[id] = data.Bytes()
	f.types[id] = head.GetMediaType()
	return stream.SendAndClose(&runtimev1.ArtifactPutObjectResponseV1{Info: &runtimev1.ArtifactObjectInfoV1{Key: head.GetKey(), SizeBytes: int64(data.Len())}})
}

func (f *fakeService) GetObject(request *runtimev1.ArtifactGetObjectRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactGetObjectResponseV1]) error {
	ref := request.GetObject()
	if err := f.authorize(stream.Context(), ref.GetProjectId()); err != nil {
		return err
	}
	f.mu.Lock()
	data, ok := f.objects[objectID(ref.GetProjectId(), ref.GetBucket(), ref.GetKey())]
	f.mu.Unlock()
	if !ok {
		return refuse(stream.Context(), codes.NotFound, 404, "not_found", "no such object")
	}
	if err := stream.Send(&runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Info{
		Info: &runtimev1.ArtifactObjectInfoV1{Key: ref.GetKey(), SizeBytes: int64(len(data))},
	}}); err != nil {
		return err
	}
	if ref.GetKey() == f.hangKey {
		<-stream.Context().Done()
		f.hung <- struct{}{}
		return stream.Context().Err()
	}
	for offset := 0; offset < len(data); offset += 700 << 10 {
		end := min(offset+700<<10, len(data))
		if err := stream.Send(&runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Data{Data: data[offset:end]}}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeService) ListObjects(request *runtimev1.ArtifactListObjectsRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactListObjectsResponseV1]) error {
	if err := f.authorize(stream.Context(), request.GetProjectId()); err != nil {
		return err
	}
	f.mu.Lock()
	var keys []string
	prefix := objectID(request.GetProjectId(), request.GetBucket(), "")
	for id := range f.objects {
		if strings.HasPrefix(id, prefix+request.GetPrefix()) {
			keys = append(keys, strings.TrimPrefix(id, prefix))
		}
	}
	f.mu.Unlock()
	// One key per page, so the client must take every page of the stream.
	for i, key := range keys {
		next := ""
		if i < len(keys)-1 {
			next = "cursor"
		}
		f.mu.Lock()
		f.pages++
		f.mu.Unlock()
		if err := stream.Send(&runtimev1.ArtifactListObjectsResponseV1{
			Objects: []*runtimev1.ArtifactObjectInfoV1{{Key: key}}, NextCursor: next,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeService) DeleteBatch(ctx context.Context, request *runtimev1.ArtifactDeleteBatchRequestV1) (*runtimev1.ArtifactDeleteBatchResponseV1, error) {
	if err := f.authorize(ctx, request.GetProjectId()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	answer := &runtimev1.ArtifactDeleteBatchResponseV1{}
	for _, key := range request.GetKeys() {
		id := objectID(request.GetProjectId(), request.GetBucket(), key)
		if _, ok := f.objects[id]; !ok {
			answer.Failed = append(answer.Failed, &runtimev1.ArtifactDeleteFailureV1{Key: key, Code: "not_found", Message: "absent"})
			continue
		}
		delete(f.objects, id)
		answer.Deleted = append(answer.Deleted, key)
	}
	return answer, nil
}

// platform starts the fake on a real TLS 1.3 listener that requires a
// runtime-CA client certificate, as elitea-main's platform listener does, and
// returns the callback with the client identity (paths in directory).
func platform(t *testing.T, service *fakeService) (artifacts.Callback, string) {
	t.Helper()
	directory := t.TempDir()
	caKey, caCert := newTestCA(t)
	writePEM(t, filepath.Join(directory, "runtime-ca.crt"), "CERTIFICATE", caCert.Raw)
	serverPair := issueLeaf(t, caKey, caCert, x509.ExtKeyUsageServerAuth, "", "")
	issueLeaf(t, caKey, caCert, x509.ExtKeyUsageClientAuth,
		filepath.Join(directory, "callback-client.crt"), filepath.Join(directory, "callback-client.key"))
	clients := x509.NewCertPool()
	clients.AddCert(caCert)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverPair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clients,
	})))
	runtimev1.RegisterArtifactObjectServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return artifacts.Callback{
		Target:   "https://localhost:" + port,
		CAFile:   filepath.Join(directory, "runtime-ca.crt"),
		CertFile: filepath.Join(directory, "callback-client.crt"),
		KeyFile:  filepath.Join(directory, "callback-client.key"),
	}, directory
}

func client(t *testing.T, callback artifacts.Callback, settings artifacts.Settings) *artifacts.GRPCClient {
	t.Helper()
	conn, err := artifacts.Dial(callback)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	built, err := artifacts.NewClient(conn, settings)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

var authorized = artifacts.Settings{APIKey: "token", ProjectID: "7"}

// An upload of several chunks reaches the service byte-identical, with no
// media type (elitea-main types the object by its extension), overwrite on,
// the bucket lowercased and the key a path; the download streams it back
// byte-identical.
func TestUploadAndDownloadRoundTripMultiChunk(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	c := client(t, callback, authorized)
	payload := make([]byte, 3<<20+512<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Upload(ctx, "Wiki-Artifacts", "acme--x--main/wiki_pages/a/b.md", payload); err != nil {
		t.Fatal(err)
	}
	head := service.putHeads[0]
	if head.GetBucket() != "wiki-artifacts" || head.GetKey() != "acme--x--main/wiki_pages/a/b.md" ||
		!head.GetOverwrite() || head.GetMediaType() != "" || head.GetProjectId() != 7 || service.frames < 4 {
		t.Fatalf("head %v after %d frames", head, service.frames)
	}
	got, err := c.Download(ctx, "wiki-artifacts", "acme--x--main/wiki_pages/a/b.md")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("download: %d bytes, %v", len(got), err)
	}
	// An empty object is an empty object, not a missing one.
	if err := c.Upload(ctx, "wiki-artifacts", "empty.md", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Download(ctx, "wiki-artifacts", "empty.md"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty object: %v %v", got, err)
	}
}

// Download names its two refusals with sentinels and never returns an object
// cut short at the read bound.
func TestDownloadRefusesAMissingAndAnOversizedObject(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	c := client(t, callback, authorized)
	ctx := context.Background()
	if err := c.Upload(ctx, "graphs", "graph.json", []byte(`{"nodes": []}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Download(ctx, "graphs", "absent.json"); !errors.Is(err, artifacts.ErrNotFound) ||
		!strings.HasPrefix(err.Error(), "artifact not found: graphs/absent.json") {
		t.Fatalf("a missing object: %v", err)
	}
	if data, err := c.DownloadUpTo(ctx, "graphs", "graph.json", 4); !errors.Is(err, artifacts.ErrTooLarge) || data != nil {
		t.Fatalf("an oversized object: %d bytes, %v", len(data), err)
	}
}

// Every refusal carries the platform's status and text, in the Python
// client's words for uploads; a cross-project call is refused 403.
func TestRefusalsCarryThePlatformsText(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	ctx := context.Background()

	other := client(t, callback, artifacts.Settings{APIKey: "token", ProjectID: "8"})
	if err := other.Upload(ctx, "wiki", "k", []byte("x")); err == nil ||
		!strings.HasPrefix(err.Error(), "Not authorized to upload artifact (HTTP 403): forbidden: no access to project") {
		t.Fatalf("cross-project upload: %v", err)
	}
	if _, err := other.Download(ctx, "wiki", "k"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("cross-project download: %v", err)
	}
	if _, err := other.List(ctx, "wiki", ""); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("cross-project list: %v", err)
	}
	if _, _, err := other.DeleteBatch(ctx, "wiki", []string{"k"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("cross-project delete: %v", err)
	}

	stranger := client(t, callback, artifacts.Settings{APIKey: "stolen", ProjectID: "7"})
	if err := stranger.Upload(ctx, "wiki", "k", []byte("x")); err == nil ||
		!strings.HasPrefix(err.Error(), "Failed to upload artifact: HTTP 401") {
		t.Fatalf("bad bearer: %v", err)
	}
	full := client(t, callback, authorized)
	if err := full.Upload(ctx, "full", "k", []byte("x")); err == nil ||
		!strings.HasPrefix(err.Error(), "Failed to upload artifact: HTTP 507 — quota: bucket is full") {
		t.Fatalf("a refused upload: %v", err)
	}

	for _, settings := range []artifacts.Settings{{APIKey: "k"}, {ProjectID: "7"}, {APIKey: "k", ProjectID: "seven"}} {
		if _, err := artifacts.NewClient(nil, settings); err == nil {
			t.Fatalf("%+v was accepted", settings)
		}
	}
}

// The platform listener requires the runtime-CA client certificate at the
// handshake: without it no call reaches a route, and the error says the hop
// failed rather than inventing a refusal.
func TestTheHopRequiresTheCallbackClientCertificate(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	anonymous := callback
	anonymous.CertFile, anonymous.KeyFile = "", ""
	c := client(t, anonymous, authorized)
	err := c.Upload(context.Background(), "wiki", "page.md", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "HTTP") {
		t.Fatalf("an upload with no client certificate: %v", err)
	}
	if len(service.putHeads) != 0 {
		t.Fatal("the anonymous upload was stored")
	}

	for name, bad := range map[string]artifacts.Callback{
		"no address":         {TargetEnv: "ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL"},
		"a path":             {Target: "https://elitea-main:9446/api"},
		"cleartext":          {Target: "http://elitea-main:9446"},
		"no port":            {Target: "elitea-main"},
		"a lone certificate": {Target: "elitea-main:9446", CertFile: "/only-the-cert"},
	} {
		if _, err := artifacts.Dial(bad); err == nil {
			t.Errorf("%s was accepted", name)
		} else if name == "no address" && !strings.Contains(err.Error(), "ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL") {
			t.Errorf("the refusal does not name the variable: %v", err)
		}
	}
}

// List takes every page of the stream; DeleteBatch reports what survived.
func TestListAndDeleteBatch(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	c := client(t, callback, authorized)
	ctx := context.Background()
	for _, key := range []string{"w1/a.md", "w1/b.md", "w1/c.md", "w2/a.md"} {
		if err := c.Upload(ctx, "wiki", key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := c.List(ctx, "wiki", "w1/")
	if err != nil || len(listed) != 3 || service.pages != 3 {
		t.Fatalf("listed %v after %d pages: %v", listed, service.pages, err)
	}
	deleted, failed, err := c.DeleteBatch(ctx, "wiki", []string{"w1/a.md", "w1/zz.md"})
	if err != nil || len(deleted) != 1 || deleted[0] != "w1/a.md" || len(failed) != 1 || failed[0].Key != "w1/zz.md" || failed[0].Code != "not_found" {
		t.Fatalf("deleted %v failed %v: %v", deleted, failed, err)
	}
	if _, _, err := c.DeleteBatch(ctx, "wiki", nil); err == nil {
		t.Fatal("an empty batch was sent")
	}
}

// Cancelling the caller's context ends the stream on the server too.
func TestCancellingADownloadEndsTheStream(t *testing.T) {
	service := newFakeService()
	service.hangKey = "slow.bin"
	callback, _ := platform(t, service)
	c := client(t, callback, authorized)
	if err := c.Upload(context.Background(), "wiki", "slow.bin", []byte("x")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Download(ctx, "wiki", "slow.bin"); err == nil {
		t.Fatal("a cancelled download succeeded")
	}
	select {
	case <-service.hung:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the cancellation")
	}
}

// The default factory dials once and binds each invocation's bearer.
func TestTheFactoryNeedsATransportAndBindsTheBearer(t *testing.T) {
	service := newFakeService()
	callback, _ := platform(t, service)
	factory := artifacts.FactoryFor(callback,
		func(settings map[string]any) artifacts.Settings {
			return artifacts.Settings{APIKey: settings["api_key"].(string), ProjectID: "7"}
		},
		func(settings map[string]any) bool { return settings["api_key"] != nil })
	if built, err := factory(map[string]any{}); built != nil || err != nil {
		t.Fatalf("no transport: %v %v", built, err)
	}
	built, err := factory(map[string]any{"api_key": "token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := built.Upload(context.Background(), "wiki", "k.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	unset := artifacts.FactoryFor(artifacts.Callback{TargetEnv: "ELITEA_INVENTORY_ARTIFACT_SERVICE_URL"},
		func(map[string]any) artifacts.Settings { return authorized },
		func(map[string]any) bool { return true })
	if _, err := unset(nil); err == nil || !strings.Contains(err.Error(), "ELITEA_INVENTORY_ARTIFACT_SERVICE_URL") {
		t.Fatalf("an unset address: %v", err)
	}
}

func newTestCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "runtime test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, certificate
}

// issueLeaf signs a leaf; with paths it also writes the PEM pair there.
func issueLeaf(t *testing.T, caKey *ecdsa.PrivateKey, ca *x509.Certificate, usage x509.ExtKeyUsage, certPath, keyPath string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "leaf"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if certPath != "" {
		writePEM(t, certPath, "CERTIFICATE", der)
		writePEM(t, keyPath, "PRIVATE KEY", pkcs8)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

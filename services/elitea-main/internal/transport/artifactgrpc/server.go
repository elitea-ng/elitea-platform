// Package artifactgrpc serves elitea.runtime.v1.ArtifactObjectService on
// elitea-main's platform gRPC listener (#1359 U5): the agent worker's, the
// DeepWiki/Inventory engines' and the sub-application host's artifact calls,
// which were HTTP requests to /api/v2/artifacts.
//
// Each RPC is one artifact route, run in process through the public handler
// (internal/transport/platformcall): the bearer is authenticated, the
// project's artifact permission is resolved for its principal, the per-bucket
// access list and the system-bucket rules apply — the same route, middleware
// and handler a browser request reaches, not a second implementation. The
// project is the request's own and travels in the route, so a bearer of one
// project is refused another's bucket exactly as over HTTP.
//
// A refusal is a gRPC status mapped from the route's HTTP status, with the
// status, error code and message in the `elitea-artifact-refusal-bin` trailer
// (runtimev1.ArtifactRefusalV1). Who may call is decided before any of this:
// the platform listener's grpcauth.Policy admits only the identities
// configured for this service.
package artifactgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/platformcall"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// RefusalTrailer is the trailer that carries a runtimev1.ArtifactRefusalV1.
const RefusalTrailer = "elitea-artifact-refusal-bin"

// ChunkBytes is the largest object chunk GetObject sends.
const ChunkBytes = 1 << 20

const (
	// maxJSONBytes caps a JSON answer (a listing page, a bucket list).
	maxJSONBytes = 8 << 20
	// maxRefusalBytes caps the refusal body read for its code and message.
	maxRefusalBytes = 64 << 10
	// maxListPages bounds one ListObjects call, as the clients' own ceiling.
	maxListPages = 1000
	// maxNameBytes bounds a bucket, key or prefix; the routes apply their
	// own, narrower rules after this.
	maxNameBytes = 1024
	// maxBatchKeys bounds one DeleteBatch.
	maxBatchKeys = 1000
)

// The headers the artifact routes read a runtime claim from
// (v2/artifacts.RuntimeClaimHeaders).
const (
	headerClaimExecution  = "X-Elitea-Execution-Id"
	headerClaimGeneration = "X-Elitea-Execution-Generation"
	headerClaimID         = "X-Elitea-Claim-Id"
	headerClaimFence      = "X-Elitea-Fence"
)

// Server implements runtimev1.ArtifactObjectServiceServer over the public
// HTTP handler that serves /api/v2/artifacts.
type Server struct {
	runtimev1.UnimplementedArtifactObjectServiceServer
	handler http.Handler
	logger  *slog.Logger
}

// NewServer builds the service over handler, elitea-main's public handler.
func NewServer(handler http.Handler, logger *slog.Logger) (*Server, error) {
	if handler == nil {
		return nil, errors.New("artifactgrpc: the artifact handler is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{handler: handler, logger: logger}, nil
}

// PlatformService declares the service for the platform listener, admitting
// clients only.
func (s *Server) PlatformService(clients grpcauth.Allowlist) runtimegrpc.PlatformService {
	return runtimegrpc.PlatformService{
		Name:     runtimev1.ArtifactObjectService_ServiceDesc.ServiceName,
		Clients:  clients,
		Register: func(registrar grpc.ServiceRegistrar) { runtimev1.RegisterArtifactObjectServiceServer(registrar, s) },
	}
}

// ListBuckets serves GET /api/v2/artifacts/buckets/{project}.
func (s *Server) ListBuckets(ctx context.Context, request *runtimev1.ArtifactListBucketsRequestV1) (*runtimev1.ArtifactListBucketsResponseV1, error) {
	path, err := bucketsPath(request.GetProjectId())
	if err != nil {
		return nil, err
	}
	var answer struct {
		Buckets []bucketJSON `json:"buckets"`
	}
	if err := s.callJSON(ctx, http.MethodGet, path, nil, nil, nil, &answer); err != nil {
		return nil, err
	}
	response := &runtimev1.ArtifactListBucketsResponseV1{}
	for _, bucket := range answer.Buckets {
		response.Buckets = append(response.Buckets, bucket.proto())
	}
	return response, nil
}

// CreateBucket serves POST /api/v2/artifacts/buckets/{project}.
func (s *Server) CreateBucket(ctx context.Context, request *runtimev1.ArtifactCreateBucketRequestV1) (*runtimev1.ArtifactCreateBucketResponseV1, error) {
	path, err := bucketsPath(request.GetProjectId())
	if err != nil {
		return nil, err
	}
	if !validName(request.GetName()) {
		return nil, status.Error(codes.InvalidArgument, "name is not a bucket name")
	}
	body := map[string]any{"name": request.GetName()}
	if request.RetentionDays != nil {
		body["retention_days"] = request.GetRetentionDays()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, status.Error(codes.Internal, "the bucket request could not be encoded")
	}
	var bucket bucketJSON
	if err := s.callJSON(ctx, http.MethodPost, path, nil, nil, bytes.NewReader(encoded), &bucket); err != nil {
		return nil, err
	}
	return &runtimev1.ArtifactCreateBucketResponseV1{Bucket: bucket.proto()}, nil
}

// ListObjects serves GET /api/v2/artifacts/objects/{project}/{bucket}, one
// response per page, following the cursor up to max_pages.
func (s *Server) ListObjects(request *runtimev1.ArtifactListObjectsRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactListObjectsResponseV1]) error {
	ctx := stream.Context()
	path, err := objectsPath(request.GetProjectId(), request.GetBucket(), "")
	if err != nil {
		return err
	}
	claim, err := claimHeaders(request.GetClaim())
	if err != nil {
		return err
	}
	for _, value := range []string{request.GetPrefix(), request.GetDelimiter(), request.GetCursor()} {
		if len(value) > maxNameBytes || strings.ContainsRune(value, 0) {
			return status.Error(codes.InvalidArgument, "prefix, delimiter and cursor must be short and carry no NUL")
		}
	}
	if request.GetPageSize() < 0 || request.GetMaxPages() < 0 {
		return status.Error(codes.InvalidArgument, "page_size and max_pages must not be negative")
	}
	pages := int(max(request.GetMaxPages(), 1))
	if pages > maxListPages {
		pages = maxListPages
	}
	cursor := request.GetCursor()
	for page := 0; page < pages; page++ {
		query := url.Values{}
		if request.GetPrefix() != "" {
			query.Set("prefix", request.GetPrefix())
		}
		if request.GetDelimiter() != "" {
			query.Set("delimiter", request.GetDelimiter())
		}
		if request.GetPageSize() > 0 {
			query.Set("limit", strconv.Itoa(int(request.GetPageSize())))
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var answer struct {
			Objects        []objectJSON `json:"objects"`
			CommonPrefixes []string     `json:"common_prefixes"`
			NextCursor     string       `json:"next_cursor"`
		}
		if err := s.callJSON(ctx, http.MethodGet, path, query, claim, nil, &answer); err != nil {
			return err
		}
		response := &runtimev1.ArtifactListObjectsResponseV1{CommonPrefixes: answer.CommonPrefixes, NextCursor: answer.NextCursor}
		for _, object := range answer.Objects {
			response.Objects = append(response.Objects, object.proto())
		}
		if err := stream.Send(response); err != nil {
			return err
		}
		if answer.NextCursor == "" || answer.NextCursor == cursor {
			return nil
		}
		cursor = answer.NextCursor
	}
	return nil
}

// Stat serves HEAD /api/v2/artifacts/objects/{project}/{bucket}/{key}.
func (s *Server) Stat(ctx context.Context, request *runtimev1.ArtifactStatRequestV1) (*runtimev1.ArtifactStatResponseV1, error) {
	ref := request.GetObject()
	path, err := objectPath(ref)
	if err != nil {
		return nil, err
	}
	claim, err := claimHeaders(ref.GetClaim())
	if err != nil {
		return nil, err
	}
	httpRequest, err := s.request(ctx, http.MethodHead, path, nil, claim, nil)
	if err != nil {
		return nil, err
	}
	writer := platformcall.NewBuffer(maxRefusalBytes)
	if err := s.serve(ctx, writer, httpRequest); err != nil {
		return nil, err
	}
	if !platformcall.Success(writer.Status()) {
		return nil, refuse(ctx, writer.Status(), writer.Body())
	}
	return &runtimev1.ArtifactStatResponseV1{Info: infoFromHeader(ref.GetKey(), writer.Header())}, nil
}

// GetObject serves GET /api/v2/artifacts/objects/{project}/{bucket}/{key}:
// the info frame, then the bytes in chunks of at most ChunkBytes.
func (s *Server) GetObject(request *runtimev1.ArtifactGetObjectRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactGetObjectResponseV1]) error {
	ctx := stream.Context()
	ref := request.GetObject()
	path, err := objectPath(ref)
	if err != nil {
		return err
	}
	claim, err := claimHeaders(ref.GetClaim())
	if err != nil {
		return err
	}
	httpRequest, err := s.request(ctx, http.MethodGet, path, nil, claim, nil)
	if err != nil {
		return err
	}
	writer := &objectWriter{header: http.Header{}, key: ref.GetKey(), send: stream.Send}
	if err := s.serve(ctx, writer, httpRequest); err != nil {
		return err
	}
	if writer.sendErr != nil {
		if err := platformcall.ContextStatus(ctx); err != nil {
			return err
		}
		return writer.sendErr
	}
	if writer.refusal != nil {
		return refuse(ctx, writer.code, writer.refusal)
	}
	if !writer.infoSent {
		writer.WriteHeader(http.StatusOK)
		if writer.sendErr != nil {
			return writer.sendErr
		}
	}
	return writer.flush()
}

// PutObject serves POST /api/v2/artifacts/objects/{project}/{bucket} as the
// multipart upload the HTTP callers sent: one `file` part named by the key,
// with the head's media type (none derives it from the key's extension, as
// before). The bytes stream into the handler as the client sends them.
func (s *Server) PutObject(stream grpc.ClientStreamingServer[runtimev1.ArtifactPutObjectRequestV1, runtimev1.ArtifactPutObjectResponseV1]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "a PutObject call starts with its head")
		}
		return err
	}
	head := first.GetHead()
	if head == nil {
		return status.Error(codes.InvalidArgument, "a PutObject call starts with its head")
	}
	path, err := objectsPath(head.GetProjectId(), head.GetBucket(), "")
	if err != nil {
		return err
	}
	if !validKey(head.GetKey()) {
		return status.Error(codes.InvalidArgument, "key is not an object key")
	}
	if head.GetMediaType() != "" && (len(head.GetMediaType()) > 255 || !platformcall.HeaderSafe(head.GetMediaType())) {
		return status.Error(codes.InvalidArgument, "media_type is not a media type")
	}
	claim, err := claimHeaders(head.GetClaim())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	body, pipe := io.Pipe()
	form := multipart.NewWriter(pipe)
	go func() {
		part := textproto.MIMEHeader{}
		part.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": head.GetKey()}))
		if head.GetMediaType() != "" {
			part.Set("Content-Type", head.GetMediaType())
		}
		file, err := form.CreatePart(part)
		if err != nil {
			_ = pipe.CloseWithError(err)
			return
		}
		for {
			frame, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				_ = pipe.CloseWithError(form.Close())
				return
			}
			if err != nil {
				// A cancelled or broken upload ends the body with an error, so
				// the handler fails the part read and stores nothing.
				_ = pipe.CloseWithError(err)
				cancel()
				return
			}
			data, ok := frame.GetFrame().(*runtimev1.ArtifactPutObjectRequestV1_Data)
			if !ok {
				_ = pipe.CloseWithError(status.Error(codes.InvalidArgument, "a PutObject call has one head"))
				cancel()
				return
			}
			if _, err := file.Write(data.Data); err != nil {
				return
			}
		}
	}()
	defer func() { _ = body.CloseWithError(context.Canceled) }()

	query := url.Values{}
	if head.GetOverwrite() {
		query.Set("overwrite", "true")
	}
	httpRequest, err := s.request(ctx, http.MethodPost, path, query, claim, body)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", form.FormDataContentType())
	writer := platformcall.NewBuffer(maxJSONBytes)
	if err := s.serve(ctx, writer, httpRequest); err != nil {
		return err
	}
	if !platformcall.Success(writer.Status()) {
		return refuse(ctx, writer.Status(), writer.Body())
	}
	var stored objectJSON
	if err := json.Unmarshal(writer.Body(), &stored); err != nil {
		return status.Error(codes.Internal, "the upload answer is not JSON")
	}
	info := stored.proto()
	if info.GetMediaType() == "" {
		info.MediaType = head.GetMediaType()
	}
	return stream.SendAndClose(&runtimev1.ArtifactPutObjectResponseV1{Info: info})
}

// Delete serves DELETE /api/v2/artifacts/objects/{project}/{bucket}/{key}.
func (s *Server) Delete(ctx context.Context, request *runtimev1.ArtifactDeleteRequestV1) (*runtimev1.ArtifactDeleteResponseV1, error) {
	ref := request.GetObject()
	path, err := objectPath(ref)
	if err != nil {
		return nil, err
	}
	claim, err := claimHeaders(ref.GetClaim())
	if err != nil {
		return nil, err
	}
	if err := s.callJSON(ctx, http.MethodDelete, path, nil, claim, nil, nil); err != nil {
		return nil, err
	}
	return &runtimev1.ArtifactDeleteResponseV1{}, nil
}

// DeleteBatch serves POST /api/v2/artifacts/objects/{project}/{bucket}:batchDelete.
func (s *Server) DeleteBatch(ctx context.Context, request *runtimev1.ArtifactDeleteBatchRequestV1) (*runtimev1.ArtifactDeleteBatchResponseV1, error) {
	path, err := objectsPath(request.GetProjectId(), request.GetBucket(), "")
	if err != nil {
		return nil, err
	}
	if len(request.GetKeys()) > maxBatchKeys {
		return nil, status.Errorf(codes.InvalidArgument, "a batch names at most %d keys", maxBatchKeys)
	}
	claim, err := claimHeaders(request.GetClaim())
	if err != nil {
		return nil, err
	}
	keys := request.GetKeys()
	if keys == nil {
		keys = []string{}
	}
	encoded, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		return nil, status.Error(codes.Internal, "the batch could not be encoded")
	}
	var answer struct {
		Deleted []string `json:"deleted"`
		Failed  []struct {
			Key     string `json:"key"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"failed"`
	}
	if err := s.callJSON(ctx, http.MethodPost, path+":batchDelete", nil, claim, bytes.NewReader(encoded), &answer); err != nil {
		return nil, err
	}
	response := &runtimev1.ArtifactDeleteBatchResponseV1{Deleted: answer.Deleted}
	for _, failure := range answer.Failed {
		response.Failed = append(response.Failed, &runtimev1.ArtifactDeleteFailureV1{Key: failure.Key, Code: failure.Code, Message: failure.Message})
	}
	return response, nil
}

// request builds one in-process artifact request.
func (s *Server) request(ctx context.Context, method, path string, query url.Values, claim http.Header, body io.Reader) (*http.Request, error) {
	request, err := platformcall.NewRequest(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	for name, values := range claim {
		request.Header[name] = values
	}
	if body != nil && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	return request, nil
}

// serve runs the handler; a broken or cancelled call is a status.
func (s *Server) serve(ctx context.Context, writer http.ResponseWriter, request *http.Request) error {
	if aborted := platformcall.Serve(s.handler, writer, request, s.logger); aborted {
		if err := platformcall.ContextStatus(ctx); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "the artifact call broke")
	}
	return platformcall.ContextStatus(ctx)
}

// callJSON runs one request whose answer is JSON, decoding a success into
// into (nil ignores the body) and turning a refusal into its status.
func (s *Server) callJSON(ctx context.Context, method, path string, query url.Values, claim http.Header, body io.Reader, into any) error {
	request, err := s.request(ctx, method, path, query, claim, body)
	if err != nil {
		return err
	}
	writer := platformcall.NewBuffer(maxJSONBytes)
	if err := s.serve(ctx, writer, request); err != nil {
		return err
	}
	if !platformcall.Success(writer.Status()) {
		return refuse(ctx, writer.Status(), writer.Body())
	}
	if writer.TooLarge() {
		return status.Errorf(codes.ResourceExhausted, "the artifact answer exceeds %d bytes", maxJSONBytes)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(writer.Body(), into); err != nil {
		return status.Error(codes.Internal, "the artifact answer is not JSON")
	}
	return nil
}

// refuse ends a call the route refused, with the refusal trailer.
func refuse(ctx context.Context, httpStatus int, body []byte) error {
	refusal := &runtimev1.ArtifactRefusalV1{HttpStatus: int32(httpStatus)} //nolint:gosec // an HTTP status fits
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &envelope) == nil {
		refusal.Code, refusal.Message = envelope.Error.Code, envelope.Error.Message
	}
	return platformcall.Refuse(ctx, RefusalTrailer, httpStatus, refusal, "artifact call")
}

// bucketsPath is /api/v2/artifacts/buckets/{project}.
func bucketsPath(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", status.Error(codes.InvalidArgument, "project_id must be positive")
	}
	return "/api/v2/artifacts/buckets/" + strconv.FormatInt(projectID, 10), nil
}

// objectsPath is /api/v2/artifacts/objects/{project}/{bucket}[/{key}]. The
// path is routed decoded, so each value lands in its route parameter whole.
func objectsPath(projectID int64, bucket, key string) (string, error) {
	if projectID <= 0 {
		return "", status.Error(codes.InvalidArgument, "project_id must be positive")
	}
	if !validName(bucket) {
		return "", status.Error(codes.InvalidArgument, "bucket is not a bucket name")
	}
	path := "/api/v2/artifacts/objects/" + strconv.FormatInt(projectID, 10) + "/" + bucket
	if key == "" {
		return path, nil
	}
	if !validKey(key) {
		return "", status.Error(codes.InvalidArgument, "key is not an object key")
	}
	return path + "/" + key, nil
}

// objectPath is the route of one named object; the key is required.
func objectPath(ref *runtimev1.ArtifactObjectRefV1) (string, error) {
	if !validKey(ref.GetKey()) {
		return "", status.Error(codes.InvalidArgument, "key is not an object key")
	}
	return objectsPath(ref.GetProjectId(), ref.GetBucket(), ref.GetKey())
}

// validName is one path segment: the bucket route parameter. The route
// applies the bucket rules themselves.
func validName(name string) bool {
	return name != "" && len(name) <= maxNameBytes && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\:\x00") && printable(name)
}

// validKey keeps a key a key: no empty, "." or ".." segment, no backslash or
// NUL, so it cannot change which route the path reaches. The storage layer
// applies its own rules after this.
func validKey(key string) bool {
	if key == "" || len(key) > maxNameBytes || strings.ContainsAny(key, "\\\x00") || !printable(key) {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func printable(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// claimHeaders turns a runtime claim into the headers the routes read.
func claimHeaders(claim *runtimev1.ArtifactRuntimeClaimV1) (http.Header, error) {
	if claim == nil {
		return nil, nil
	}
	values := map[string]string{
		headerClaimExecution:  claim.GetExecutionId(),
		headerClaimGeneration: strconv.FormatUint(claim.GetGeneration(), 10),
		headerClaimID:         claim.GetClaimId(),
		headerClaimFence:      claim.GetFence(),
	}
	header := http.Header{}
	for name, value := range values {
		if value == "" || len(value) > 4096 || !platformcall.HeaderSafe(value) {
			return nil, status.Error(codes.InvalidArgument, "the runtime claim is not a set of header values")
		}
		header.Set(name, value)
	}
	return header, nil
}

// infoFromHeader reads an object's info from a HEAD or GET answer's headers.
func infoFromHeader(key string, header http.Header) *runtimev1.ArtifactObjectInfoV1 {
	info := &runtimev1.ArtifactObjectInfoV1{
		Key:       key,
		MediaType: header.Get("Content-Type"),
		Etag:      header.Get("ETag"),
	}
	if size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && size >= 0 {
		info.SizeBytes = size
	}
	if modified, err := http.ParseTime(header.Get("Last-Modified")); err == nil {
		info.ModifiedAt = modified.UTC().Format(time.RFC3339)
	}
	return info
}

// bucketJSON is the bucket the routes answer (v2/artifacts.Bucket).
type bucketJSON struct {
	Name          string `json:"name"`
	RetentionDays *int32 `json:"retention_days"`
	SizeBytes     int64  `json:"size_bytes"`
	ObjectCount   int64  `json:"object_count"`
}

func (b bucketJSON) proto() *runtimev1.ArtifactBucketV1 {
	return &runtimev1.ArtifactBucketV1{Name: b.Name, RetentionDays: b.RetentionDays, SizeBytes: b.SizeBytes, ObjectCount: b.ObjectCount}
}

// objectJSON is a listed or stored object as the routes answer it.
type objectJSON struct {
	Key        string `json:"key"`
	SizeBytes  int64  `json:"size_bytes"`
	MediaType  string `json:"media_type"`
	ETag       string `json:"etag"`
	ModifiedAt string `json:"modified_at"`
	UpdatedAt  string `json:"updated_at"`
}

func (o objectJSON) proto() *runtimev1.ArtifactObjectInfoV1 {
	modified := o.ModifiedAt
	if modified == "" {
		modified = o.UpdatedAt
	}
	return &runtimev1.ArtifactObjectInfoV1{Key: o.Key, SizeBytes: o.SizeBytes, MediaType: o.MediaType, Etag: o.ETag, ModifiedAt: modified}
}

// objectWriter is the http.ResponseWriter of GetObject: a 2xx head becomes
// the info frame, the body becomes chunks of at most ChunkBytes; a refusal's
// body is kept for the trailer.
type objectWriter struct {
	header http.Header
	key    string
	send   func(*runtimev1.ArtifactGetObjectResponseV1) error

	code     int
	infoSent bool
	pending  []byte
	refusal  []byte
	sendErr  error
}

func (w *objectWriter) Header() http.Header { return w.header }

func (w *objectWriter) WriteHeader(code int) {
	if w.code != 0 || (code >= 100 && code <= 199) {
		return
	}
	w.code = code
	if !platformcall.Success(code) {
		w.refusal = []byte{}
		return
	}
	w.infoSent = true
	info := infoFromHeader(w.key, w.header)
	if err := w.send(&runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Info{Info: info}}); err != nil {
		w.sendErr = err
	}
}

func (w *objectWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	if w.refusal != nil {
		room := maxRefusalBytes - len(w.refusal)
		if room > 0 {
			w.refusal = append(w.refusal, data[:min(room, len(data))]...)
		}
		return len(data), nil
	}
	w.pending = append(w.pending, data...)
	for len(w.pending) >= ChunkBytes {
		if err := w.sendChunk(w.pending[:ChunkBytes]); err != nil {
			return 0, err
		}
		w.pending = w.pending[ChunkBytes:]
	}
	return len(data), nil
}

// Flush sends what is buffered, so a slow store's bytes are not held back.
func (w *objectWriter) Flush() {
	if w.sendErr == nil && w.refusal == nil {
		_ = w.flush()
	}
}

func (w *objectWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	err := w.sendChunk(w.pending)
	w.pending = nil
	return err
}

func (w *objectWriter) sendChunk(chunk []byte) error {
	// The frame owns its bytes: pending is reused.
	frame := &runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Data{Data: append([]byte(nil), chunk...)}}
	if err := w.send(frame); err != nil {
		w.sendErr = err
		return err
	}
	return nil
}

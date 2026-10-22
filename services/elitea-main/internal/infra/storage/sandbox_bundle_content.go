package storage

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const sandboxBundleGrantDomain = "elitea.sandbox.job-grant.ed25519.v1\x00"

// maxSandboxGrantBytes bounds the protobuf encoding of one signed job grant.
const maxSandboxGrantBytes = 8192

// nativeSandboxFileLimit bounds one native bundle file upload.
const nativeSandboxFileLimit = 128 * 1024 * 1024

// SandboxBundleKeys is the existing runtime keyring contract. The service never
// holds a signing key or accepts a key supplied by a request.
type SandboxBundleKeys interface {
	ResolveEd25519PublicKey(context.Context, string) (ed25519.PublicKey, error)
}

// SandboxBundleContentService serves only content, under fresh Main authority.
// It is reached through RuntimeContentService on the verified-client-certificate
// gRPC listener. Metadata is published last.
type SandboxBundleContentService struct {
	store     *SandboxBundleStore
	keys      SandboxBundleKeys
	audiences map[string]struct{}
	now       func() time.Time
	requests  chan struct{}
}

func NewSandboxBundleContentService(store *SandboxBundleStore, keys SandboxBundleKeys, audiences []string, now func() time.Time) (*SandboxBundleContentService, error) {
	if store == nil || keys == nil || now == nil || len(audiences) == 0 || len(audiences) > 16 {
		return nil, errors.New("sandbox content requires storage, trusted keys, exact recipients, and a clock")
	}
	allowed := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		if !sandboxClaimIdentity(audience) {
			return nil, errors.New("sandbox content recipient is invalid")
		}
		allowed[audience] = struct{}{}
	}
	return &SandboxBundleContentService{store: store, keys: keys, audiences: allowed, now: now, requests: make(chan struct{}, 4)}, nil
}

func sandboxClaimIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

// verifiedSandboxGrant checks the caller is one of the exact recipients and
// returns the claims bytes of a grant signed by a trusted runtime key.
func verifiedSandboxGrant(ctx context.Context, keys SandboxBundleKeys, audiences map[string]struct{}, native []byte) (string, []byte, error) {
	connection, ok := contentPeer(ctx)
	if !ok {
		return "", nil, ErrContentUnauthorized
	}
	peer, err := workloadIdentity(connection.PeerCertificates[0])
	if err != nil {
		return "", nil, ErrContentUnauthorized
	}
	if _, ok := audiences[peer]; !ok {
		return "", nil, ErrContentUnauthorized
	}
	if len(native) == 0 || len(native) > maxSandboxGrantBytes {
		return "", nil, ErrContentUnauthorized
	}
	grant := &runtimev1.SignedSandboxJobGrantV1{}
	if runtimegrpc.ScanStrictMessage(native, grant.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(native, grant) != nil || !sandboxClaimIdentity(grant.KeyId) || len(grant.Signature) != ed25519.SignatureSize || len(grant.ClaimsBytes) == 0 || len(grant.ClaimsBytes) > 4096 {
		return "", nil, ErrContentUnauthorized
	}
	key, err := keys.ResolveEd25519PublicKey(ctx, grant.KeyId)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return "", nil, ErrContentUnauthorized
	}
	input := make([]byte, len(sandboxBundleGrantDomain)+8+len(grant.ClaimsBytes))
	offset := copy(input, sandboxBundleGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(grant.ClaimsBytes)))
	copy(input[offset+8:], grant.ClaimsBytes)
	if !ed25519.Verify(key, input, grant.Signature) {
		return "", nil, ErrContentUnauthorized
	}
	return peer, grant.ClaimsBytes, nil
}

func (service *SandboxBundleContentService) authorize(ctx context.Context, root string, native []byte) (SandboxBundleScope, *runtimev1.SandboxJobGrantClaimsV1, error) {
	if !sandboxDigest(root) {
		return SandboxBundleScope{}, nil, ErrContentUnauthorized
	}
	peer, raw, err := verifiedSandboxGrant(ctx, service.keys, service.audiences, native)
	if err != nil {
		return SandboxBundleScope{}, nil, err
	}
	claims := &runtimev1.SandboxJobGrantClaimsV1{}
	if runtimegrpc.ScanStrictMessage(raw, claims.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(raw, claims) != nil {
		return SandboxBundleScope{}, nil, ErrContentUnauthorized
	}
	now := service.now().UnixMilli()
	if claims.Revision != 3 || claims.CancelOnly || claims.Audience != peer || claims.Generation == 0 || !sandboxClaimIdentity(claims.TenantId) || !sandboxClaimIdentity(claims.ExecutionId) || !sandboxClaimIdentity(claims.ActivationId) || !sandboxClaimIdentity(claims.SubmitterWorkloadIdentity) || len(claims.RequestDigest) != 32 || len(claims.DependencyBundleSha256) != 32 || hex.EncodeToString(claims.DependencyBundleSha256) != root || claims.IssuedAtUnixMillis > now || claims.ExpiresAtUnixMillis <= now || claims.IssuedAtUnixMillis < 0 || claims.ExpiresAtUnixMillis-claims.IssuedAtUnixMillis > 30000 {
		return SandboxBundleScope{}, nil, ErrContentUnauthorized
	}
	scope, err := NewSandboxBundleScope(claims.TenantId, claims.ProjectId)
	if err != nil {
		return SandboxBundleScope{}, nil, err
	}
	return scope, claims, nil
}

// parse admits bundle metadata of the requested kind. A native bundle is also
// bound to the preparation the grant was issued for.
func parseSandboxBundle(kind runtimev1.SandboxBundleKindV1, native []byte, root string, claims *runtimev1.SandboxJobGrantClaimsV1) (sandboxStoredBundle, error) {
	switch kind {
	case runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_PYTHON:
		bundle, err := ParsePythonSandboxBundle(native, root)
		if err != nil {
			return nil, err
		}
		return bundle, nil
	case runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_NATIVE:
		bundle, err := ParseNativeSandboxBundle(native, root)
		if err != nil {
			return nil, err
		}
		if hex.EncodeToString(claims.RequestDigest) != bundle.record.Preparation {
			return nil, ErrContentUnauthorized
		}
		return bundle, nil
	}
	return nil, ErrContentRejected
}

func (service *SandboxBundleContentService) open(ctx context.Context, kind runtimev1.SandboxBundleKindV1, scope SandboxBundleScope, root string) (sandboxStoredBundle, error) {
	switch kind {
	case runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_PYTHON:
		bundle, err := service.store.OpenBundle(ctx, scope, root)
		if err != nil {
			return nil, err
		}
		return bundle, nil
	case runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_NATIVE:
		bundle, err := service.store.OpenNativeBundle(ctx, scope, root)
		if err != nil {
			return nil, err
		}
		return bundle, nil
	}
	return nil, ErrContentRejected
}

func sandboxFileLimit(kind runtimev1.SandboxBundleKindV1) int64 {
	if kind == runtimev1.SandboxBundleKindV1_SANDBOX_BUNDLE_KIND_V1_NATIVE {
		return nativeSandboxFileLimit
	}
	return sandboxBundleFileLimit
}

// GetSandboxBundle returns the published metadata of one prepared bundle.
func (s *ContentServer) GetSandboxBundle(ctx context.Context, request *runtimev1.GetSandboxBundleRequestV1) (*runtimev1.GetSandboxBundleResponseV1, error) {
	service := s.sandboxBundles
	if service == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireSandboxSlot(service.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	scope, _, err := service.authorize(ctx, request.GetRoot(), request.GetSignedGrant())
	if err != nil {
		return nil, sandboxBundleStatus(ctx, err)
	}
	bundle, err := service.open(ctx, request.GetKind(), scope, request.GetRoot())
	if err != nil {
		return nil, sandboxBundleStatus(ctx, err)
	}
	return &runtimev1.GetSandboxBundleResponseV1{BundleJson: bundle.nativeBytes()}, nil
}

// PublishSandboxBundle publishes bundle metadata after every file is stored.
func (s *ContentServer) PublishSandboxBundle(ctx context.Context, request *runtimev1.PublishSandboxBundleRequestV1) (*runtimev1.PublishSandboxBundleResponseV1, error) {
	service := s.sandboxBundles
	if service == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireSandboxSlot(service.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	scope, claims, err := service.authorize(ctx, request.GetRoot(), request.GetSignedGrant())
	if err != nil {
		return nil, sandboxBundleStatus(ctx, err)
	}
	native := request.GetBundleJson()
	if len(native) == 0 || len(native) > sandboxBundleMetadataLimit {
		return nil, sandboxBundleStatus(ctx, ErrContentRejected)
	}
	bundle, err := parseSandboxBundle(request.GetKind(), native, request.GetRoot(), claims)
	if err == nil {
		err = service.store.Publish(ctx, scope, bundle)
	}
	if err != nil {
		return nil, sandboxBundleStatus(ctx, err)
	}
	return &runtimev1.PublishSandboxBundleResponseV1{}, nil
}

// UploadSandboxBundleFile stores one verified bundle file: a header, then
// chunks. Nothing is stored unless length and digest match the metadata.
func (s *ContentServer) UploadSandboxBundleFile(stream grpc.ClientStreamingServer[runtimev1.UploadSandboxBundleFileRequestV1, runtimev1.UploadSandboxBundleFileResponseV1]) error {
	service := s.sandboxBundles
	if service == nil {
		return contentStatus(codes.Unimplemented)
	}
	ctx := stream.Context()
	release, err := acquireSandboxSlot(service.requests)
	if err != nil {
		return err
	}
	defer release()
	first, err := stream.Recv()
	if err != nil {
		return sandboxBundleStatus(ctx, ErrContentRejected)
	}
	header := first.GetHeader()
	if header == nil {
		return sandboxBundleStatus(ctx, ErrContentRejected)
	}
	scope, claims, err := service.authorize(ctx, header.GetRoot(), header.GetSignedGrant())
	if err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	native := header.GetBundleJson()
	if len(native) == 0 || len(native) > sandboxBundleMetadataLimit {
		return sandboxBundleStatus(ctx, ErrContentRejected)
	}
	bundle, err := parseSandboxBundle(header.GetKind(), native, header.GetRoot(), claims)
	if err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	if _, err := bundle.file(header.GetName()); err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	body := newUploadChunkReader(func() ([]byte, bool, error) {
		message, err := stream.Recv()
		if err != nil {
			return nil, false, err
		}
		chunk, ok := message.GetPart().(*runtimev1.UploadSandboxBundleFileRequestV1_Chunk)
		return chunkBytes(chunk, ok), ok, nil
	}, sandboxFileLimit(header.GetKind()))
	if err := service.store.PutFile(ctx, scope, bundle, header.GetName(), body); err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	// Bytes already written are verified content under this exact root.
	// Refuse trailing data; completion is never reported for a malformed upload.
	if err := body.finish(); err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	return stream.SendAndClose(&runtimev1.UploadSandboxBundleFileResponseV1{})
}

func chunkBytes(chunk *runtimev1.UploadSandboxBundleFileRequestV1_Chunk, ok bool) []byte {
	if !ok || chunk == nil {
		return nil
	}
	return chunk.Chunk
}

// FetchSandboxBundleFile streams one verified bundle file: a header with its
// length and digest, then chunks. Storage is verified before the header.
func (s *ContentServer) FetchSandboxBundleFile(request *runtimev1.FetchSandboxBundleFileRequestV1, stream grpc.ServerStreamingServer[runtimev1.FetchSandboxBundleFileResponseV1]) error {
	service := s.sandboxBundles
	if service == nil {
		return contentStatus(codes.Unimplemented)
	}
	ctx := stream.Context()
	release, err := acquireSandboxSlot(service.requests)
	if err != nil {
		return err
	}
	defer release()
	scope, _, err := service.authorize(ctx, request.GetRoot(), request.GetSignedGrant())
	if err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	bundle, err := service.open(ctx, request.GetKind(), scope, request.GetRoot())
	if err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	file, err := bundle.file(request.GetName())
	if err != nil {
		return sandboxBundleStatus(ctx, err)
	}
	return sendStagedFile(ctx, service.store.spoolDir, func(output io.Writer) (int64, string, error) {
		return file.Bytes, file.SHA256, service.store.FetchFile(ctx, scope, bundle, file.Name, output)
	}, func(header *runtimev1.SandboxContentFileHeaderV1, chunk []byte) error {
		if header != nil {
			return stream.Send(&runtimev1.FetchSandboxBundleFileResponseV1{Part: &runtimev1.FetchSandboxBundleFileResponseV1_Header{Header: header}})
		}
		return stream.Send(&runtimev1.FetchSandboxBundleFileResponseV1{Part: &runtimev1.FetchSandboxBundleFileResponseV1_Chunk{Chunk: chunk}})
	}, sandboxBundleStatus)
}

// sendStagedFile fetches into private staging, so corrupt storage can never
// yield a header, then streams the verified bytes.
func sendStagedFile(ctx context.Context, spoolDir string, fetch func(io.Writer) (int64, string, error), send func(*runtimev1.SandboxContentFileHeaderV1, []byte) error, failure func(context.Context, error) error) error {
	staged, err := os.CreateTemp(spoolDir, "delivery-*")
	if err != nil {
		return failure(ctx, err)
	}
	defer func() {
		if err := errors.Join(staged.Close(), os.Remove(staged.Name())); err != nil {
			slog.ErrorContext(ctx, "sandbox content delivery staging cleanup failed")
		}
	}()
	size, digest, err := fetch(staged)
	if err != nil {
		return failure(ctx, err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return failure(ctx, err)
	}
	hash, err := hex.DecodeString(digest)
	if err != nil || len(hash) != 32 || size < 0 {
		return failure(ctx, ErrContentRejected)
	}
	if err := send(&runtimev1.SandboxContentFileHeaderV1{Length: uint64(size), Sha256: hash}, nil); err != nil {
		return err
	}
	buffer := make([]byte, contentChunkBytes)
	reader := sandboxContextReader{ctx, staged}
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if sendErr := send(nil, append([]byte(nil), buffer[:count]...)); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			slog.ErrorContext(ctx, "sandbox content file delivery failed")
			return failure(ctx, err)
		}
	}
}

// uploadChunkReader turns the chunk messages of a client stream into a
// bounded reader. A non-chunk message, an empty or oversized chunk, or more
// bytes than the limit refuses the upload.
type uploadChunkReader struct {
	recv   func() ([]byte, bool, error)
	buffer []byte
	limit  int64
	read   int64
	done   bool
}

func newUploadChunkReader(recv func() ([]byte, bool, error), limit int64) *uploadChunkReader {
	return &uploadChunkReader{recv: recv, limit: limit}
}

func (r *uploadChunkReader) Read(p []byte) (int, error) {
	for len(r.buffer) == 0 {
		if r.done {
			return 0, io.EOF
		}
		chunk, ok, err := r.recv()
		if errors.Is(err, io.EOF) {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		if !ok || len(chunk) == 0 || len(chunk) > contentChunkBytes {
			return 0, ErrContentRejected
		}
		r.read += int64(len(chunk))
		if r.read > r.limit {
			return 0, ErrContentRejected
		}
		r.buffer = chunk
	}
	count := copy(p, r.buffer)
	r.buffer = r.buffer[count:]
	return count, nil
}

// finish requires the stream to end exactly where the verified file ended.
func (r *uploadChunkReader) finish() error {
	if len(r.buffer) != 0 {
		return ErrContentRejected
	}
	var probe [1]byte
	count, err := r.Read(probe[:])
	if count != 0 {
		return ErrContentRejected
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func acquireSandboxSlot(slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, grpcstatus.Error(codes.Unavailable, "Package transfer capacity is full. Retry the same preparation identity.")
	}
}

func sandboxBundleStatus(ctx context.Context, err error) error {
	code, message := codes.Unavailable, "Shared package storage failed. Retry the same preparation identity."
	switch {
	case errors.Is(err, ErrContentUnauthorized), errors.Is(err, ErrAccessDenied):
		code, message = codes.PermissionDenied, "The package storage grant is invalid, expired, or belongs to another workload."
	case errors.Is(err, ErrContentRejected):
		code, message = codes.FailedPrecondition, "Package metadata or content does not match the admitted bundle."
	case errors.Is(err, ErrNotFound):
		code, message = codes.NotFound, "The prepared package bundle is incomplete or unavailable."
	case errors.Is(err, ErrContentUnavailable):
		code, message = codes.Unavailable, "Package transfer capacity is full. Retry the same preparation identity."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code, message = codes.DeadlineExceeded, "Package transfer was interrupted. Retry the same preparation identity."
	default:
		slog.ErrorContext(ctx, "sandbox bundle transfer failed", "reason", message)
	}
	return grpcstatus.Error(code, message)
}

// WithSandboxBundles must be called before Register. Nil leaves the RPCs
// unimplemented.
func (server *ContentServer) WithSandboxBundles(service *SandboxBundleContentService) *ContentServer {
	server.sandboxBundles = service
	return server
}

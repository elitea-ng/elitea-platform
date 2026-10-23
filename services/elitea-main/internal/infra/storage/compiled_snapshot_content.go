package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type CompiledSnapshotContentService struct {
	store     *SandboxBundleStore
	index     domain.RustSnapshotIndex
	keys      SandboxBundleKeys
	audiences map[string]struct{}
	now       func() time.Time
	requests  chan struct{}
}

func NewCompiledSnapshotContentService(store *SandboxBundleStore, index domain.RustSnapshotIndex, keys SandboxBundleKeys, audiences []string, now func() time.Time) (*CompiledSnapshotContentService, error) {
	if store == nil || index == nil || keys == nil || now == nil || len(audiences) == 0 || len(audiences) > 16 {
		return nil, domain.ErrSnapshotInvalid
	}
	allowed := map[string]struct{}{}
	for _, a := range audiences {
		if !sandboxClaimIdentity(a) {
			return nil, domain.ErrSnapshotInvalid
		}
		allowed[a] = struct{}{}
	}
	return &CompiledSnapshotContentService{store, index, keys, allowed, now, make(chan struct{}, 4)}, nil
}
func (s *CompiledSnapshotContentService) authorize(ctx context.Context, root string, native []byte, publishing bool) (*runtimev1.RustCompiledSnapshotGrantClaimsV1, error) {
	if !domain.SnapshotDigest(root) {
		return nil, ErrContentUnauthorized
	}
	peer, raw, err := verifiedSandboxGrant(ctx, s.keys, s.audiences, native)
	if err != nil {
		return nil, err
	}
	c := &runtimev1.RustCompiledSnapshotGrantClaimsV1{}
	if runtimegrpc.ScanStrictMessage(raw, c.ProtoReflect().Descriptor()) != nil || proto.Unmarshal(raw, c) != nil {
		return nil, ErrContentUnauthorized
	}
	now := s.now().UnixMilli()
	if c.Revision != 4 || c.Audience != peer || c.Generation == 0 || !sandboxClaimIdentity(c.TenantId) || !sandboxClaimIdentity(c.ExecutionId) || !sandboxClaimIdentity(c.ActivationId) || !sandboxClaimIdentity(c.SubmitterWorkloadIdentity) || c.ProjectId <= 0 || len(c.RequestDigest) != 32 || len(c.BasePreparedRequestSha256) != 32 || len(c.SnapshotKeySha256) != 32 || len(c.DescriptorSha256) != 32 || hex.EncodeToString(c.DescriptorSha256) != root || c.IssuedAtUnixMillis < 0 || c.IssuedAtUnixMillis > now || c.ExpiresAtUnixMillis <= now || c.ExpiresAtUnixMillis-c.IssuedAtUnixMillis > 30000 {
		return nil, ErrContentUnauthorized
	}
	if publishing {
		if c.Purpose != runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH || len(c.CompilationJobKey) != 32 || c.CompilationRuntimeId == "" || len(c.CompilationRuntimeId) > 512 || len(c.CompilationRequestDigest) != 32 || c.CompilationLeaseEpoch == 0 || domain.SnapshotActivationKey(c.ExecutionId, c.ActivationId) != hex.EncodeToString(c.CompilationJobKey) {
			return nil, ErrContentUnauthorized
		}
	} else if c.Purpose != runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ || len(c.CompilationJobKey) != 0 || c.CompilationRuntimeId != "" || len(c.CompilationRequestDigest) != 0 || c.CompilationLeaseEpoch != 0 {
		return nil, ErrContentUnauthorized
	}
	return c, nil
}

// admission authorizes one call and bounds it by the grant's own expiry.
func (s *CompiledSnapshotContentService) admission(ctx context.Context, root string, grant []byte, publishing bool) (context.Context, *runtimev1.RustCompiledSnapshotGrantClaimsV1, context.CancelFunc, error) {
	c, err := s.authorize(ctx, root, grant, publishing)
	if err != nil {
		return ctx, nil, func() {}, err
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(c.ExpiresAtUnixMillis))
	return ctx, c, cancel, nil
}

func compiledScope(c *runtimev1.RustCompiledSnapshotGrantClaimsV1) domain.SnapshotScope {
	return domain.SnapshotScope{TenantID: c.TenantId, ProjectID: c.ProjectId}
}

func compiledBindingClaims(candidate domain.SnapshotCandidate, c *runtimev1.RustCompiledSnapshotGrantClaimsV1) error {
	d, err := candidate.Descriptor()
	if err != nil || candidate.Root != hex.EncodeToString(c.DescriptorSha256) || candidate.Key != hex.EncodeToString(c.SnapshotKeySha256) || d.Binding.BasePreparedRequestSHA256 != hex.EncodeToString(c.BasePreparedRequestSha256) {
		return ErrContentUnauthorized
	}
	expected, err := domain.SnapshotJobDigest("execute", d.Binding, candidate.Root)
	if c.Purpose == runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH {
		expected = candidate.CompilationRequestDigest
		if candidate.CompilationJobKey != hex.EncodeToString(c.CompilationJobKey) || candidate.CompilationRequestDigest != hex.EncodeToString(c.CompilationRequestDigest) || candidate.CompilationRuntimeID != c.CompilationRuntimeId || candidate.CompilationLeaseEpoch != c.CompilationLeaseEpoch {
			return ErrContentUnauthorized
		}
	}
	if err != nil || expected != hex.EncodeToString(c.RequestDigest) {
		return ErrContentUnauthorized
	}
	return nil
}

func (s *CompiledSnapshotContentService) publicationCandidate(ctx context.Context, c *runtimev1.RustCompiledSnapshotGrantClaimsV1, complete bool) (domain.SnapshotCandidate, error) {
	candidate, err := s.index.Candidate(ctx, compiledScope(c), hex.EncodeToString(c.SnapshotKeySha256), hex.EncodeToString(c.CompilationJobKey), complete)
	if err != nil {
		return candidate, err
	}
	if !complete && candidate.ReceiptSHA256 == "" && !candidate.CurrentLeaseLive {
		return candidate, domain.ErrSnapshotUnavailable
	}
	return candidate, compiledBindingClaims(candidate, c)
}

// readyBundle opens the ready snapshot the read grant names.
func (s *CompiledSnapshotContentService) readyBundle(ctx context.Context, c *runtimev1.RustCompiledSnapshotGrantClaimsV1, use func(SandboxBundleScope, *RustCompiledSnapshot) error) error {
	return s.index.WithReady(ctx, compiledScope(c), hex.EncodeToString(c.SnapshotKeySha256), hex.EncodeToString(c.DescriptorSha256), func(candidate domain.SnapshotCandidate) error {
		if err := compiledBindingClaims(candidate, c); err != nil {
			return err
		}
		scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
		bundle, err := s.store.OpenCompiledSnapshot(ctx, scope, candidate.Root)
		if err != nil {
			return err
		}
		if !bytes.Equal(bundle.native, candidate.DescriptorJSON) {
			return ErrContentRejected
		}
		return use(scope, bundle)
	})
}

// GetCompiledSnapshot returns the descriptor of one ready compiled snapshot.
func (server *ContentServer) GetCompiledSnapshot(ctx context.Context, request *runtimev1.GetCompiledSnapshotRequestV1) (*runtimev1.GetCompiledSnapshotResponseV1, error) {
	s := server.compiledSnapshots
	if s == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireCompiledSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, c, cancel, err := s.admission(ctx, request.GetRoot(), request.GetSignedGrant(), false)
	defer cancel()
	if err != nil {
		return nil, compiledStatus(err)
	}
	var body []byte
	err = s.readyBundle(ctx, c, func(_ SandboxBundleScope, bundle *RustCompiledSnapshot) error {
		body = bundle.native
		return nil
	})
	if err != nil {
		return nil, compiledStatus(err)
	}
	return &runtimev1.GetCompiledSnapshotResponseV1{DescriptorJson: body}, nil
}

// UploadCompiledSnapshotFile stores the executable of the snapshot the
// original compilation reserved: a header, then chunks.
func (server *ContentServer) UploadCompiledSnapshotFile(stream grpc.ClientStreamingServer[runtimev1.UploadCompiledSnapshotFileRequestV1, runtimev1.UploadCompiledSnapshotFileResponseV1]) error {
	s := server.compiledSnapshots
	if s == nil {
		return contentStatus(codes.Unimplemented)
	}
	release, err := acquireCompiledSlot(s.requests)
	if err != nil {
		return err
	}
	defer release()
	first, err := stream.Recv()
	if err != nil || first.GetHeader() == nil {
		return compiledStatus(ErrContentRejected)
	}
	header := first.GetHeader()
	ctx, c, cancel, err := s.admission(stream.Context(), header.GetRoot(), header.GetSignedGrant(), true)
	defer cancel()
	if err != nil {
		return compiledStatus(err)
	}
	if header.GetName() != SnapshotExecutableName {
		return compiledStatus(ErrContentRejected)
	}
	candidate, err := s.publicationCandidate(ctx, c, false)
	if err != nil {
		return compiledStatus(err)
	}
	if err = s.index.Reserve(ctx, candidate); err != nil {
		return compiledStatus(err)
	}
	raw := header.GetDescriptorJson()
	if len(raw) > domain.SnapshotDescriptorLimit || !bytes.Equal(raw, candidate.DescriptorJSON) {
		return compiledStatus(ErrContentRejected)
	}
	bundle, err := ParseRustCompiledSnapshot(raw, candidate.Root)
	if err != nil {
		return compiledStatus(err)
	}
	body := newUploadChunkReader(func() ([]byte, bool, error) {
		message, err := stream.Recv()
		if err != nil {
			return nil, false, err
		}
		chunk, ok := message.GetPart().(*runtimev1.UploadCompiledSnapshotFileRequestV1_Chunk)
		if !ok || chunk == nil {
			return nil, false, nil
		}
		return chunk.Chunk, true, nil
	}, domain.SnapshotExecutableLimit)
	scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
	err = s.index.WithPublishing(ctx, candidate, func() error {
		if err := s.store.PutFile(ctx, scope, bundle, SnapshotExecutableName, body); err != nil {
			return err
		}
		return body.finish()
	})
	if err != nil {
		return compiledStatus(err)
	}
	return stream.SendAndClose(&runtimev1.UploadCompiledSnapshotFileResponseV1{})
}

// PublishCompiledSnapshot commits the descriptor after the executable.
func (server *ContentServer) PublishCompiledSnapshot(ctx context.Context, request *runtimev1.PublishCompiledSnapshotRequestV1) (*runtimev1.PublishCompiledSnapshotResponseV1, error) {
	s := server.compiledSnapshots
	if s == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireCompiledSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, c, cancel, err := s.admission(ctx, request.GetRoot(), request.GetSignedGrant(), true)
	defer cancel()
	if err != nil {
		return nil, compiledStatus(err)
	}
	raw := request.GetDescriptorJson()
	if len(raw) < 1 || len(raw) > domain.SnapshotDescriptorLimit {
		return nil, compiledStatus(ErrContentRejected)
	}
	candidate, err := s.publicationCandidate(ctx, c, true)
	if err != nil || !bytes.Equal(raw, candidate.DescriptorJSON) {
		if err == nil {
			err = ErrContentRejected
		}
		return nil, compiledStatus(err)
	}
	bundle, err := ParseRustCompiledSnapshot(raw, candidate.Root)
	if err != nil {
		return nil, compiledStatus(err)
	}
	scope, _ := NewSandboxBundleScope(c.TenantId, c.ProjectId)
	if err = s.index.CommitReady(ctx, candidate, func() error { return s.store.Publish(ctx, scope, bundle) }); err != nil {
		return nil, compiledStatus(err)
	}
	return &runtimev1.PublishCompiledSnapshotResponseV1{}, nil
}

// FetchCompiledSnapshotFile streams the verified executable of one ready
// snapshot: a header with its length and digest, then chunks.
func (server *ContentServer) FetchCompiledSnapshotFile(request *runtimev1.FetchCompiledSnapshotFileRequestV1, stream grpc.ServerStreamingServer[runtimev1.FetchCompiledSnapshotFileResponseV1]) error {
	s := server.compiledSnapshots
	if s == nil {
		return contentStatus(codes.Unimplemented)
	}
	release, err := acquireCompiledSlot(s.requests)
	if err != nil {
		return err
	}
	defer release()
	ctx, c, cancel, err := s.admission(stream.Context(), request.GetRoot(), request.GetSignedGrant(), false)
	defer cancel()
	if err != nil {
		return compiledStatus(err)
	}
	if request.GetName() != SnapshotExecutableName {
		return compiledStatus(ErrContentRejected)
	}
	return sendStagedFile(ctx, s.store.spoolDir, func(output io.Writer) (int64, string, error) {
		var size int64
		var digest string
		err := s.readyBundle(ctx, c, func(scope SandboxBundleScope, bundle *RustCompiledSnapshot) error {
			size = bundle.descriptor.ExecutableBytes
			digest = bundle.descriptor.ExecutableSHA256
			return s.store.FetchFile(ctx, scope, bundle, SnapshotExecutableName, output)
		})
		return size, digest, err
	}, func(header *runtimev1.SandboxContentFileHeaderV1, chunk []byte) error {
		if header != nil {
			return stream.Send(&runtimev1.FetchCompiledSnapshotFileResponseV1{Part: &runtimev1.FetchCompiledSnapshotFileResponseV1_Header{Header: header}})
		}
		return stream.Send(&runtimev1.FetchCompiledSnapshotFileResponseV1{Part: &runtimev1.FetchCompiledSnapshotFileResponseV1_Chunk{Chunk: chunk}})
	}, func(_ context.Context, err error) error { return compiledStatus(err) })
}

func acquireCompiledSlot(slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, compiledStatus(ErrContentUnavailable)
	}
}

func compiledStatus(err error) error {
	code, message := codes.Unavailable, "Compiled snapshot storage is unavailable."
	switch {
	case errors.Is(err, ErrContentUnauthorized), errors.Is(err, ErrAccessDenied):
		code, message = codes.PermissionDenied, "The compiled snapshot grant is not accepted."
	case errors.Is(err, ErrContentRejected), errors.Is(err, domain.ErrSnapshotInvalid):
		code, message = codes.FailedPrecondition, "Compiled snapshot content does not match its immutable descriptor."
	case errors.Is(err, domain.ErrSnapshotConflict), errors.Is(err, domain.ErrSnapshotUnavailable), errors.Is(err, domain.ErrSnapshotMiss):
		code, message = codes.Aborted, "The original compilation or selected snapshot is not ready."
	case errors.Is(err, domain.ErrSnapshotQuota), errors.Is(err, ErrContentUnavailable):
		code, message = codes.Unavailable, "Compiled snapshot capacity is full."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code, message = codes.DeadlineExceeded, "Compiled snapshot transfer was interrupted."
	}
	return grpcstatus.Error(code, message)
}

// WithCompiledSnapshots must be called before Register. Nil leaves the RPCs
// unimplemented.
func (s *ContentServer) WithCompiledSnapshots(service *CompiledSnapshotContentService) *ContentServer {
	s.compiledSnapshots = service
	return s
}

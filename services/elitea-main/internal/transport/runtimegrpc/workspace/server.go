// Package workspace serves the cloud executor's workspace-store calls
// (elitea.runtime.v1.WorkspaceStoreService, ADR-0034 decisions 9 and 14) on
// Main's private control listener.
//
// Every call is bound to the live claim: the peer is authenticated (mTLS
// workload identity), the fence verified live, and only then is the claim
// handed to the workspaceruntime.Store, which resolves it to its handoff
// thread. The thread and the project are the claim's, never the request's.
//
// A nil Store is the "feature unavailable" seam: the service is registered,
// every call answers UNSUPPORTED_CAPABILITY, and nothing reaches storage.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	// ChunkBytes bounds every bytes frame in both directions, under the
	// control listener's 64 KiB request and 80 KiB response bounds.
	ChunkBytes = 32 << 10
	// maxClaimTextBytes bounds the claim id.
	maxClaimTextBytes = 256
)

// WorkloadAuthorizer is control.WorkloadAuthorizer: the peer identity from
// the verified mTLS session, compared with the persisted workload session.
type WorkloadAuthorizer interface {
	AuthorizeWorkload(ctx context.Context, workloadSessionID, producerID string) (workloadIdentity string, err error)
}

// ClaimVerifier is the live-fence check of the claim controller.
type ClaimVerifier interface {
	VerifyActive(ctx context.Context, fence runtimedomain.Fence) error
}

// Server implements runtimev1.WorkspaceStoreServiceServer.
type Server struct {
	runtimev1.UnimplementedWorkspaceStoreServiceServer

	authorizer WorkloadAuthorizer
	claims     ClaimVerifier
	store      workspaceruntime.Store
	logger     *slog.Logger
}

// NewServer builds the service. store may be nil (feature unavailable);
// the authorizer and the claim verifier may not.
func NewServer(authorizer WorkloadAuthorizer, claims ClaimVerifier, store workspaceruntime.Store, logger *slog.Logger) (*Server, error) {
	if authorizer == nil || claims == nil {
		return nil, errors.New("workspace store service requires the workload authorizer and the claim verifier")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{authorizer: authorizer, claims: claims, store: store, logger: logger}, nil
}

// Available reports whether a Store is composed.
func (s *Server) Available() bool { return s != nil && s.store != nil }

var errUnavailableFeature = errors.New("workspace store is not served")

// authorize turns a wire claim into a verified ClaimRef, or a safe rejection.
func (s *Server) authorize(ctx context.Context, claim *runtimev1.WorkspaceClaimV1) (workspaceruntime.ClaimRef, *runtimev1.RuntimeErrorV1) {
	if claim == nil || claim.GetIdentity() == nil || claim.GetFence() == nil ||
		!boundedText(claim.GetClaimId(), maxClaimTextBytes) ||
		!workspaceruntime.ValidThreadUUID(claim.GetThreadUuid()) || claim.GetEpoch() == 0 {
		return workspaceruntime.ClaimRef{}, rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The workspace request is malformed.", false)
	}
	if s.store == nil {
		return workspaceruntime.ClaimRef{}, rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "The workspace store is not served by this deployment.", false)
	}
	identity, err := s.authorizer.AuthorizeWorkload(ctx, claim.GetFence().GetWorkloadSessionId(), claim.GetFence().GetProducerId())
	if err != nil || identity == "" {
		return workspaceruntime.ClaimRef{}, rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false)
	}
	fence, err := fenceDomain(claim.GetIdentity(), claim.GetFence(), identity)
	if err != nil {
		return workspaceruntime.ClaimRef{}, rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false)
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		return workspaceruntime.ClaimRef{}, fenceRejection(err)
	}
	return workspaceruntime.ClaimRef{
		ExecutionID:      fence.ExecutionID,
		Generation:       fence.Generation,
		ClaimID:          claim.GetClaimId(),
		FenceToken:       append([]byte(nil), fence.Token[:]...),
		WorkloadIdentity: fence.WorkloadIdentity,
		ThreadUUID:       claim.GetThreadUuid(),
		Epoch:            claim.GetEpoch(),
	}, nil
}

// WorkspaceFetchPlan streams the plan, then each segment's chunks and end.
func (s *Server) WorkspaceFetchPlan(request *runtimev1.WorkspaceFetchPlanRequestV1, stream runtimev1.WorkspaceStoreService_WorkspaceFetchPlanServer) error {
	ctx := stream.Context()
	if request == nil {
		return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Rejection{Rejection: malformed()}})
	}
	claim, reject := s.authorize(ctx, request.GetClaim())
	if reject != nil {
		return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Rejection{Rejection: reject}})
	}
	plan, err := s.store.FetchPlan(ctx, claim, request.GetSinceSeq())
	if err != nil {
		return s.sendFetchFailure(ctx, stream, "fetch_plan", err)
	}
	if err := validatePlan(plan); err != nil {
		s.logger.ErrorContext(ctx, "workspace fetch plan is invalid", "execution_id", claim.ExecutionID)
		return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Rejection{Rejection: rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The workspace fetch plan is unavailable.", false)}})
	}
	if err := stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Plan{Plan: planProto(plan)}}); err != nil {
		return err
	}
	for _, segment := range plan.Segments {
		if err := s.streamSegment(ctx, stream, claim, segment); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) streamSegment(ctx context.Context, stream runtimev1.WorkspaceStoreService_WorkspaceFetchPlanServer, claim workspaceruntime.ClaimRef, segment workspaceruntime.Segment) error {
	body, err := s.store.OpenSegment(ctx, claim, segment.SHA256)
	if err != nil {
		// The plan frame is out already; a later failure ends the stream with
		// a rejection frame the worker reads as "nothing usable".
		return s.sendFetchFailure(ctx, stream, "open_segment", err)
	}
	defer func() { _ = body.Close() }()
	hash := sha256.New()
	var sent uint64
	buffer := make([]byte, ChunkBytes)
	for {
		read, readErr := io.ReadFull(body, buffer)
		if read > 0 {
			sent += uint64(read)
			if sent > segment.Bytes {
				return s.sendIntegrity(ctx, stream, claim)
			}
			hash.Write(buffer[:read])
			chunk := append([]byte(nil), buffer[:read]...)
			if err := stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_SegmentChunk{SegmentChunk: chunk}}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return s.sendFetchFailure(ctx, stream, "read_segment", errors.Join(workspaceruntime.ErrUnavailable, readErr))
		}
	}
	if sent != segment.Bytes || hex.EncodeToString(hash.Sum(nil)) != segment.SHA256 {
		return s.sendIntegrity(ctx, stream, claim)
	}
	return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_SegmentEnd{SegmentEnd: &runtimev1.WorkspaceSegmentEndV1{Sha256: segment.SHA256, Bytes: segment.Bytes}}})
}

func (s *Server) sendIntegrity(ctx context.Context, stream runtimev1.WorkspaceStoreService_WorkspaceFetchPlanServer, claim workspaceruntime.ClaimRef) error {
	s.logger.ErrorContext(ctx, "workspace segment does not match its content address", "execution_id", claim.ExecutionID)
	return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Refusal{Refusal: &runtimev1.WorkspaceRefusalV1{Code: runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_CHECKSUM_MISMATCH}}})
}

func (s *Server) sendFetchFailure(ctx context.Context, stream runtimev1.WorkspaceStoreService_WorkspaceFetchPlanServer, stage string, err error) error {
	if refusal, ok := refusalProto(err); ok {
		return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Refusal{Refusal: refusal}})
	}
	return stream.Send(&runtimev1.WorkspaceFetchFrameV1{Frame: &runtimev1.WorkspaceFetchFrameV1_Rejection{Rejection: s.storeRejection(ctx, stage, err)}})
}

// WorkspaceUpload reads one header then the chunks, and hands the bytes to
// the Store as they arrive (never buffered whole).
func (s *Server) WorkspaceUpload(stream runtimev1.WorkspaceStoreService_WorkspaceUploadServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil || !workspaceruntime.ValidSHA256(header.GetSha256()) || header.GetBytes() == 0 || header.GetBytes() > workspaceruntime.MaxSegmentBytes {
		return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Rejection: malformed()})
	}
	claim, reject := s.authorize(ctx, header.GetClaim())
	if reject != nil {
		return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Rejection: reject})
	}
	reader := &chunkReader{stream: stream, limit: header.GetBytes()}
	result, err := s.store.PutSegment(ctx, claim, header.GetSha256(), header.GetBytes(), reader)
	if reader.protocolErr != nil {
		return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Rejection: malformed()})
	}
	if err != nil {
		if refusal, ok := refusalProto(err); ok {
			return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Refusal: refusal})
		}
		return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Rejection: s.storeRejection(ctx, "put_segment", err)})
	}
	// A Store that returned without reading every frame must not leave the
	// rest unread: drain, and refuse a stream that is longer than declared.
	if err := reader.drain(); err != nil {
		return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Rejection: malformed()})
	}
	return stream.SendAndClose(&runtimev1.WorkspaceUploadResponseV1{Exists: result.Exists})
}

// chunkReader adapts the client stream to an io.Reader over chunk frames.
type chunkReader struct {
	stream      runtimev1.WorkspaceStoreService_WorkspaceUploadServer
	pending     []byte
	read        uint64
	limit       uint64
	done        bool
	protocolErr error
}

var errUploadProtocol = errors.New("workspace upload stream is malformed")

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		frame, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		chunk := frame.GetChunk()
		if frame.GetHeader() != nil || len(chunk) == 0 || len(chunk) > ChunkBytes {
			r.protocolErr = errUploadProtocol
			return 0, errUploadProtocol
		}
		r.read += uint64(len(chunk))
		if r.read > r.limit {
			r.protocolErr = errUploadProtocol
			return 0, errUploadProtocol
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *chunkReader) drain() error {
	buffer := make([]byte, ChunkBytes)
	for {
		_, err := r.Read(buffer)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// WorkspacePublish moves refs/elitea/cloud with its expected old value.
func (s *Server) WorkspacePublish(ctx context.Context, request *runtimev1.WorkspacePublishRequestV1) (*runtimev1.WorkspacePublishResponseV1, error) {
	if request == nil {
		return &runtimev1.WorkspacePublishResponseV1{Rejection: malformed()}, nil
	}
	publish := workspaceruntime.PublishRequest{
		Kind:          request.GetKind(),
		SegmentSHA256: request.GetSegmentSha256(),
		Bytes:         request.GetBytes(),
		Prerequisites: request.GetPrerequisites(),
		ObjectFormat:  request.GetObjectFormat(),
	}
	for _, update := range request.GetRefs() {
		var old *string
		if update.Old != nil {
			value := update.GetOld()
			old = &value
		}
		publish.Refs = append(publish.Refs, workspaceruntime.RefUpdate{Name: update.GetName(), Old: old, New: update.GetNew()})
	}
	if err := workspaceruntime.ValidateExecutorPublish(publish); err != nil {
		if refusal, ok := refusalProto(err); ok {
			return &runtimev1.WorkspacePublishResponseV1{Refusal: refusal}, nil
		}
		return &runtimev1.WorkspacePublishResponseV1{Rejection: malformed()}, nil
	}
	claim, reject := s.authorize(ctx, request.GetClaim())
	if reject != nil {
		return &runtimev1.WorkspacePublishResponseV1{Rejection: reject}, nil
	}
	result, err := s.store.Publish(ctx, claim, publish)
	if err != nil {
		if refusal, ok := refusalProto(err); ok {
			return &runtimev1.WorkspacePublishResponseV1{Refusal: refusal}, nil
		}
		return &runtimev1.WorkspacePublishResponseV1{Rejection: s.storeRejection(ctx, "publish", err)}, nil
	}
	return &runtimev1.WorkspacePublishResponseV1{Seq: result.Seq, Refs: refsProto(result.Refs)}, nil
}

// maxEnvFrameBytes keeps one env frame under the control listener's 80 KiB
// response bound; a single variable is at most MaxEnvBytes (64 KiB).
const maxEnvFrameBytes = 70 << 10

// WorkspaceEnvSecret streams the thread's harvested env (contract 6.4). It
// is the only path the values take to the worker; they are never logged.
func (s *Server) WorkspaceEnvSecret(request *runtimev1.WorkspaceEnvSecretRequestV1, stream runtimev1.WorkspaceStoreService_WorkspaceEnvSecretServer) error {
	ctx := stream.Context()
	if request == nil {
		return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Rejection{Rejection: malformed()}})
	}
	claim, reject := s.authorize(ctx, request.GetClaim())
	if reject != nil {
		return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Rejection{Rejection: reject}})
	}
	env, err := s.store.EnvSecret(ctx, claim)
	if err != nil {
		if refusal, ok := refusalProto(err); ok {
			return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Refusal{Refusal: refusal}})
		}
		return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Rejection{Rejection: s.storeRejection(ctx, "env_secret", err)}})
	}
	if !workspaceruntime.ValidEnv(env) {
		s.logger.ErrorContext(ctx, "the stored workspace env is outside the contract limits", "execution_id", claim.ExecutionID)
		return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Rejection{Rejection: rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The workspace env is unavailable.", false)}})
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	frame := &runtimev1.WorkspaceEnvVarsV1{}
	size := 0
	flush := func() error {
		if len(frame.Vars) == 0 {
			return nil
		}
		err := stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_Vars{Vars: frame}})
		frame, size = &runtimev1.WorkspaceEnvVarsV1{}, 0
		return err
	}
	for _, name := range names {
		entry := len(name) + len(env[name]) + 16
		if size+entry > maxEnvFrameBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		frame.Vars = append(frame.Vars, &runtimev1.WorkspaceEnvVarV1{Name: name, Value: env[name]})
		size += entry
	}
	if err := flush(); err != nil {
		return err
	}
	return stream.Send(&runtimev1.WorkspaceEnvSecretFrameV1{Frame: &runtimev1.WorkspaceEnvSecretFrameV1_EndCount{EndCount: uint32(len(names))}})
}

func (s *Server) storeRejection(ctx context.Context, stage string, err error) *runtimev1.RuntimeErrorV1 {
	switch {
	case errors.Is(err, workspaceruntime.ErrClaimUnauthorized):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "The claim is not authorized for the workspace store.", false)
	case errors.Is(err, workspaceruntime.ErrInvalidRequest):
		return malformed()
	case errors.Is(err, runtimedomain.ErrStaleFence), errors.Is(err, runtimedomain.ErrLeaseExpired):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE, "The execution fence is no longer current.", false)
	case errors.Is(err, context.Canceled):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "The workspace operation was cancelled.", false)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, workspaceruntime.ErrUnavailable):
		s.logger.WarnContext(ctx, "workspace store unavailable", "stage", stage)
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The workspace store is temporarily unavailable.", true)
	case errors.Is(err, errUnavailableFeature):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "The workspace store is not served by this deployment.", false)
	}
	// Only the stage is logged: error text may carry driver or storage detail.
	s.logger.ErrorContext(ctx, "workspace store operation failed", "stage", stage)
	return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The workspace store operation failed.", false)
}

func fenceRejection(err error) *runtimev1.RuntimeErrorV1 {
	switch {
	case errors.Is(err, runtimedomain.ErrStaleFence), errors.Is(err, runtimedomain.ErrLeaseExpired):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE, "The execution fence is no longer current.", false)
	case errors.Is(err, runtimedomain.ErrInvalidFence):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false)
	case errors.Is(err, context.Canceled):
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "The workspace operation was cancelled.", false)
	default:
		return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The durable claim state is temporarily unavailable.", true)
	}
}

var refusalCodes = map[workspaceruntime.RefusalCode]runtimev1.WorkspaceRefusalCodeV1{
	workspaceruntime.RefusalNotWorkspaceTurn:        runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_NOT_A_WORKSPACE_TURN,
	workspaceruntime.RefusalStaleEpoch:              runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH,
	workspaceruntime.RefusalRefConflict:             runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT,
	workspaceruntime.RefusalRefNotAllowed:           runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_NOT_ALLOWED,
	workspaceruntime.RefusalSegmentMissing:          runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_SEGMENT_MISSING,
	workspaceruntime.RefusalChecksumMismatch:        runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_CHECKSUM_MISMATCH,
	workspaceruntime.RefusalUnsupportedObjectFormat: runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_UNSUPPORTED_OBJECT_FORMAT,
	workspaceruntime.RefusalSegmentTooLarge:         runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_SEGMENT_TOO_LARGE,
	workspaceruntime.RefusalQuotaExceeded:           runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_QUOTA_EXCEEDED,
}

func refusalProto(err error) (*runtimev1.WorkspaceRefusalV1, bool) {
	refusal, ok := workspaceruntime.AsRefusal(err)
	if !ok {
		return nil, false
	}
	code, known := refusalCodes[refusal.Code]
	if !known {
		return nil, false
	}
	return &runtimev1.WorkspaceRefusalV1{Code: code, Current: refsProto(refusal.Current)}, true
}

func validatePlan(plan workspaceruntime.FetchPlan) error {
	for _, ref := range plan.Refs {
		if !validRefName(ref.Name) || !workspaceruntime.ValidSHA1(ref.SHA) {
			return workspaceruntime.ErrInvalidRequest
		}
	}
	for _, segment := range plan.Segments {
		if segment.Kind != workspaceruntime.KindCompact || !workspaceruntime.ValidSHA256(segment.SHA256) ||
			segment.Bytes == 0 || segment.Bytes > workspaceruntime.MaxSegmentBytes || len(segment.Prerequisites) != 0 {
			return workspaceruntime.ErrInvalidRequest
		}
		for _, ref := range segment.Refs {
			if !validRefName(ref.Name) || !workspaceruntime.ValidSHA1(ref.SHA) {
				return workspaceruntime.ErrInvalidRequest
			}
		}
	}
	return nil
}

func validRefName(name string) bool {
	return strings.HasPrefix(name, "refs/elitea/") && boundedText(name, 255) && !strings.Contains(name, "..")
}

func planProto(plan workspaceruntime.FetchPlan) *runtimev1.WorkspaceFetchPlanV1 {
	out := &runtimev1.WorkspaceFetchPlanV1{HeadSeq: plan.HeadSeq, Refs: refsProto(plan.Refs)}
	for _, segment := range plan.Segments {
		out.Segments = append(out.Segments, &runtimev1.WorkspaceSegmentV1{
			Seq: segment.Seq, Kind: segment.Kind, Sha256: segment.SHA256, Bytes: segment.Bytes,
			Prerequisites: append([]string(nil), segment.Prerequisites...), Refs: refsProto(segment.Refs),
		})
	}
	return out
}

func refsProto(refs []workspaceruntime.Ref) []*runtimev1.WorkspaceRefV1 {
	out := make([]*runtimev1.WorkspaceRefV1, 0, len(refs))
	for _, ref := range refs {
		out = append(out, &runtimev1.WorkspaceRefV1{Name: ref.Name, Sha: ref.SHA})
	}
	return out
}

func fenceDomain(identity *runtimev1.ExecutionIdentityV1, fence *runtimev1.ExecutionFenceV1, workloadIdentity string) (runtimedomain.Fence, error) {
	if identity == nil || fence == nil || workloadIdentity == "" || len(fence.GetFenceToken()) != sha256.Size {
		return runtimedomain.Fence{}, runtimedomain.ErrInvalidFence
	}
	result := runtimedomain.Fence{
		CommandID:         identity.GetCommandId(),
		ExecutionID:       identity.GetExecutionId(),
		Generation:        identity.GetGeneration(),
		WorkloadIdentity:  workloadIdentity,
		WorkloadSessionID: fence.GetWorkloadSessionId(),
		ProducerID:        fence.GetProducerId(),
		ClaimAttempt:      fence.GetClaimAttempt(),
		LeaseEpoch:        fence.GetLeaseEpoch(),
	}
	copy(result.Token[:], fence.GetFenceToken())
	if err := result.Validate(); err != nil {
		return runtimedomain.Fence{}, err
	}
	return result, nil
}

func rejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.RuntimeErrorV1 {
	return &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: message, Retryable: retryable}
}

func malformed() *runtimev1.RuntimeErrorV1 {
	return rejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The workspace request is malformed.", false)
}

func boundedText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

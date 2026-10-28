package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"strings"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	defaultMaxInputContentBytes = 256 * 1024
	defaultMaxContentRequests   = 16
	maxRuntimeGeneration        = uint64(1<<63 - 1)
	// runtimeContextFieldSecretsHeader is the accept-list name
	// (GetEliteaClientTokenRequestV1.accept_fields) for the project's
	// `X-SECRET` value (#408).
	runtimeContextFieldSecretsHeader = "secrets-header-value"
)

var (
	ErrContentUnauthorized = errors.New("input content authorization failed")
	ErrContentNotFound     = errors.New("input content not found")
	ErrContentRejected     = errors.New("input content materialization rejected")
	ErrContentUnavailable  = errors.New("input content materialization unavailable")
	// ErrSystemBucketNameTaken is a platform system bucket name (the
	// attachment or index result bucket) held by a bucket a person made
	// before the names were reserved (review round 3). No data migration
	// moves it: the platform refuses to use it, with a message that says how
	// to free the name.
	ErrSystemBucketNameTaken = errors.New("a bucket of this platform-reserved name exists but is not a system bucket; rename or delete it so the platform can create its own")
)

// ContentClaim is the complete claim-bound authorization input. The workload
// identity comes only from the verified peer certificate, never a forwarded
// user/project header.
type ContentClaim struct {
	PeerCertificate  *x509.Certificate
	ExecutionID      string
	Generation       uint64
	ClaimID          string
	FenceToken       []byte
	ContentID        string
	ImmutableVersion string
}

type ContentAuthorization struct {
	InspectionOnly    bool
	ResourceProjectID string
	ToolkitType       string
	// ActorID is the exact durable execution_jobs.actor_id. Materializers own
	// any capability-specific interpretation, including current user lookup.
	ActorID           string
	InputBundleID     string
	CapabilityID      string
	SemanticRole      string
	ExpectedMediaType string
	ExpectedDigest    [sha256.Size]byte
	ExpectedLength    int64
}

// ContentAuthorizer validates workload identity, claim, generation, fence,
// content identity, and project ownership against the durable control state.
type ContentAuthorizer interface {
	AuthorizeContent(context.Context, ContentClaim) (ContentAuthorization, error)
}

// ContentStore opens immutable content only after authorization. Implementors
// must scope lookup by the authorized resource project.
type ContentStore interface {
	OpenContent(context.Context, string, string, string, string) (io.ReadCloser, error)
}

// ContentMaterializer derives one claim-scoped response from already verified
// immutable source bytes. It must not persist or publish the returned bytes.
// The source digest/version remain the durable execution input identity.
type ContentMaterializer interface {
	MaterializeContent(context.Context, ContentAuthorization, []byte, int64) ([]byte, error)
}

type ContentServer struct {
	// Every RPC is implemented; the embedding only satisfies forward compatibility.
	runtimev1.UnimplementedRuntimeContentServiceServer

	codePlatformPump    *CodePlatformPump
	runtimeHTTPActions  *RuntimeHTTPActionService
	codeWorkspaces      *CodeWorkspaceService
	codeWorkspaceReads  *CodeWorkspaceReadServer
	codeDebug           *RuntimeCodeDebugArtifactService
	originalCodeIntents OriginalCodeIntentStore
	nodeRecovery        NodeRecoveryControlStore
	executionInterrupts ExecutionInterruptControlStore
	nodeRecoveryResults NodeRecoveryResultStore
	authorizer          ContentAuthorizer
	store               ContentStore
	materializer        ContentMaterializer
	runtimeToken        *EliteaClientTokenService
	runtimeVersions     *RuntimeApplicationVersionService
	runtimeObjects      *RuntimeAttachmentObjectService
	toolkitArtifacts    ToolkitDiscoveryArtifactStore
	runtimeBuilders     *RuntimeEntityBuilderService
	runtimeArtifacts    *RuntimeArtifactObjectService
	sandboxBundles      *SandboxBundleContentService
	compiledSnapshots   *CompiledSnapshotContentService
	maxBytes            int64
	requests            chan struct{}
	// attachmentRequests is the attachment route's OWN pool. One attachment
	// read can wait up to attachmentExtractionWait for an extraction, so in
	// the shared pool a burst of first-time PDFs would answer 503 to every
	// token, version and artifact request on the listener.
	attachmentRequests chan struct{}
	logger             *slog.Logger
	// credentialBroker serves RedeemCredential (#1118); nil answers
	// Unimplemented. governedPins narrows a governed run's client-token
	// response; nil reads no pin.
	credentialBroker CredentialRedeemer
	governedPins     profile.PinReader
}

func NewContentServer(authorizer ContentAuthorizer, store ContentStore, maxBytes int64) (*ContentServer, error) {
	return NewContentServerWithLimits(authorizer, store, maxBytes, defaultMaxContentRequests)
}

func NewContentServerWithLimits(authorizer ContentAuthorizer, store ContentStore, maxBytes int64, maxConcurrentRequests int) (*ContentServer, error) {
	return newContentServer(authorizer, store, nil, nil, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewMaterializingContentServerWithLimits extends the same private mTLS,
// claim, fence and audience authorization path with last-moment materialization.
// It intentionally does not expose a second authentication contract.
func NewMaterializingContentServerWithLimits(authorizer ContentAuthorizer, store ContentStore, materializer ContentMaterializer, maxBytes int64, maxConcurrentRequests int) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	return newContentServer(authorizer, store, materializer, nil, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewRuntimeContentServerWithLimits enables the private Elitea client-token
// compatibility context on the same bounded mTLS listener as immutable input
// reads. Configuration materialization is deliberately not composed here: it
// belongs behind the generic Configurations domain, not an index/provider
// specific switch.
func NewRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	runtimeToken *EliteaClientTokenService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	return newContentServer(authorizer, store, nil, runtimeToken, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewMaterializingRuntimeContentServerWithLimits composes generic
// claim-scoped materialization and Elitea client-token compatibility on one
// bounded private mTLS listener.
func NewMaterializingRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	return newContentServer(authorizer, store, materializer, runtimeToken, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewNestedAgentRuntimeContentServerWithLimits adds the nested application
// (agent-as-tool) definition route to the same bounded private mTLS listener.
//
// It is a separate constructor rather than an extra argument on the one above
// because the nested route is only meaningful where agent execution is
// dispatched: the index path composes the identical listener and must not grow
// a route it can never authorize. A nil version service leaves the route
// unregistered, which is what the index composition wants and what every
// pre-existing caller keeps.
func NewNestedAgentRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	if runtimeVersions == nil {
		return nil, errors.New("runtime application version context is required")
	}
	return newContentServer(
		authorizer,
		store,
		materializer,
		runtimeToken,
		runtimeVersions,
		nil,
		maxBytes,
		maxConcurrentRequests,
	)
}

// NewAgentAttachmentRuntimeContentServerWithLimits adds the claim-scoped
// attachment OBJECT-READ route on top of the nested-agent listener above.
//
// It is a third constructor rather than a nil-able argument on the second for
// the same reason the second exists: the route must be absent, not merely
// unreachable, wherever it cannot be served. It needs object storage, and this
// service runs in deployments where the Go artifacts capability is off and
// dependencies.ObjectStore is nil (mixed deployments keep Centry's artifacts
// authoritative — see runtimecomposition's retention-sweep note). Making the
// dependency required here, and choosing the constructor at composition time,
// means a nil store yields a listener with no attachment route instead of one
// whose route answers 503 forever.
func NewAgentAttachmentRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	runtimeObjects *RuntimeAttachmentObjectService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	if runtimeVersions == nil {
		return nil, errors.New("runtime application version context is required")
	}
	if runtimeObjects == nil {
		return nil, errors.New("runtime attachment object context is required")
	}
	return newContentServer(
		authorizer,
		store,
		materializer,
		runtimeToken,
		runtimeVersions,
		runtimeObjects,
		maxBytes,
		maxConcurrentRequests,
	)
}

func newContentServer(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	runtimeObjects *RuntimeAttachmentObjectService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if authorizer == nil || store == nil {
		return nil, errors.New("content authorizer and store are required")
	}
	if maxBytes == 0 {
		maxBytes = defaultMaxInputContentBytes
	}
	if maxBytes < 1 || maxConcurrentRequests < 1 || maxConcurrentRequests > 1024 {
		return nil, errors.New("content size and concurrency limits must be positive and bounded")
	}
	return &ContentServer{
		authorizer:         authorizer,
		store:              store,
		materializer:       materializer,
		runtimeToken:       runtimeToken,
		runtimeVersions:    runtimeVersions,
		runtimeObjects:     runtimeObjects,
		maxBytes:           maxBytes,
		requests:           make(chan struct{}, maxConcurrentRequests),
		attachmentRequests: make(chan struct{}, maxConcurrentRequests),
		logger:             slog.Default(),
	}, nil
}

// WithRuntimeEntityBuilders enables the two chat-authored builder WRITE routes
// (#940 A8) on this listener.
//
// It is a post-construction setter rather than a sixth constructor, and that
// is a deliberate break with the pattern the five above established. Each of
// those exists because its route must be ABSENT, not merely unreachable,
// wherever its dependency is missing — and the same is true here — but the
// combinatorics had already reached three call-site branches over two optional
// services; a fourth optional service would have made it six, each one a
// separately-maintained argument list that differs from its neighbours by one
// nil. This setter keeps the same property (a nil service leaves both routes
// unregistered) with one line at each composition site.
//
// It must be called before Routes(). Both composition sites do
// (internal/runtimecomposition/composition.go), and a later call is harmless
// but silently useless, which is why this returns the server: the intended
// shape is one chained expression, not a statement somebody can drift away
// from its constructor.
func (s *ContentServer) WithRuntimeEntityBuilders(builders *RuntimeEntityBuilderService) *ContentServer {
	if s == nil {
		return nil
	}
	s.runtimeBuilders = builders
	return s
}

// WithRuntimeArtifacts enables the four claim-scoped `artifact` toolkit routes
// (#906) on this listener: list, read, write and delete, inside the claimed
// project and under the claimed actor's own per-bucket access list.
//
// Same shape and same reason as WithRuntimeEntityBuilders above: a nil service
// leaves all four routes UNREGISTERED rather than serving a route that answers
// 503 forever. That is not a detail here — this service runs in deployments
// with no Go object store at all (mixed deployments keep Centry's artifacts
// authoritative), and there the honest answer is that the native runtime skips
// the toolkit, not that it has one that always fails.
//
// It must be called before Routes(), which both composition sites do.
func (s *ContentServer) WithRuntimeArtifacts(artifacts *RuntimeArtifactObjectService) *ContentServer {
	if s == nil {
		return nil
	}
	s.runtimeArtifacts = artifacts
	return s
}

func (s *ContentServer) WithCodeDebugArtifacts(debug *RuntimeCodeDebugArtifactService) *ContentServer {
	if s != nil {
		s.codeDebug = debug
	}
	return s
}

// Register registers the content plane on server: the gRPC
// elitea.runtime.v1.RuntimeContentService of the content listener (#1359 U4,
// ELITEA_RUNTIME_CONTENT_ADDRESS). Every With* setter must run before it.
// An RPC whose dependency was never composed answers Unimplemented, as the
// HTTPS listener this replaced left its route unregistered.
func (s *ContentServer) Register(server grpc.ServiceRegistrar) {
	runtimev1.RegisterRuntimeContentServiceServer(server, s)
}

// contentChunkBytes is the payload of one chunk on the content plane's
// streams: small enough that a chunk never approaches the message caps,
// large enough that a 32 MiB file is a few hundred messages.
const contentChunkBytes = 64 * 1024

// GetInput streams one claim-bound, source-verified agent input: one header,
// then chunks. The whole input is read and verified (length and digest)
// before the header is sent, so corrupt storage never yields a stream that
// starts successfully.
func (s *ContentServer) GetInput(request *runtimev1.GetInputRequestV1, stream grpc.ServerStreamingServer[runtimev1.GetInputResponseV1]) error {
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return err
	}
	defer release()
	ctx := stream.Context()
	claim, err := parseContentClaim(ctx, request.GetScope(), request.GetContentId(), request.GetImmutableVersion())
	if err != nil {
		return contentStatus(codes.Unauthenticated)
	}
	if isSecurityContextContentID(claim.ContentID) {
		return s.getSecurityContext(ctx, claim, stream)
	}
	authorization, err := s.authorizer.AuthorizeContent(ctx, claim)
	if err != nil || authorization.ResourceProjectID == "" || authorization.InputBundleID == "" || authorization.CapabilityID == "" || authorization.SemanticRole == "" || authorization.ExpectedMediaType == "" || authorization.ExpectedLength <= 0 {
		return contentStatus(codes.PermissionDenied)
	}
	mediaType, parameters, err := mime.ParseMediaType(authorization.ExpectedMediaType)
	if err != nil || mediaType != authorization.ExpectedMediaType || len(parameters) != 0 {
		return contentStatus(codes.PermissionDenied)
	}
	if authorization.ExpectedLength > s.maxBytes {
		s.logInputFailure(ctx, claim, "source_limit", codes.ResourceExhausted, nil)
		return contentStatus(codes.ResourceExhausted)
	}

	content, err := s.store.OpenContent(
		ctx,
		authorization.ResourceProjectID,
		authorization.InputBundleID,
		claim.ContentID,
		claim.ImmutableVersion,
	)
	if errors.Is(err, ErrContentNotFound) {
		s.logInputFailure(ctx, claim, "source_open", codes.NotFound, err)
		return contentStatus(codes.NotFound)
	}
	if err != nil {
		s.logInputFailure(ctx, claim, "source_open", codes.Internal, err)
		return contentStatus(codes.Internal)
	}
	defer func() {
		if closeErr := content.Close(); closeErr != nil {
			s.logger.WarnContext(ctx, "input content close failed")
		}
	}()

	// This validation slice is deliberately small. Buffering here lets the
	// server verify length/digest before sending any bytes to the worker; larger
	// artifact paths use a separately resumable streaming contract.
	data, err := io.ReadAll(io.LimitReader(content, s.maxBytes+1))
	if err != nil || int64(len(data)) != authorization.ExpectedLength || int64(len(data)) > s.maxBytes {
		s.logInputFailure(ctx, claim, "source_read", codes.Internal, err)
		return contentStatus(codes.Internal)
	}
	digest := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(digest[:], authorization.ExpectedDigest[:]) != 1 {
		s.logInputFailure(ctx, claim, "source_digest", codes.Internal, nil)
		return contentStatus(codes.Internal)
	}

	responseData := data
	if s.materializer != nil && !authorization.InspectionOnly {
		responseData, err = s.materializer.MaterializeContent(ctx, authorization, data, s.maxBytes)
		if err != nil {
			code := codes.Unavailable
			if errors.Is(err, ErrContentRejected) {
				code = codes.FailedPrecondition
			}
			s.logInputFailure(ctx, claim, "materialization", code, err)
			return contentStatus(code)
		}
		if len(responseData) == 0 || int64(len(responseData)) > s.maxBytes {
			s.logInputFailure(ctx, claim, "materialization_size", codes.FailedPrecondition, nil)
			return contentStatus(codes.FailedPrecondition)
		}
	}
	responseDigest := sha256.Sum256(responseData)
	defer clearContentBytes(responseData)

	if err := stream.Send(&runtimev1.GetInputResponseV1{Part: &runtimev1.GetInputResponseV1_Header{Header: &runtimev1.InputContentHeaderV1{
		MediaType:              authorization.ExpectedMediaType,
		Length:                 uint64(len(responseData)),
		Sha256:                 responseDigest[:],
		SourceSha256:           digest[:],
		SourceLength:           uint64(authorization.ExpectedLength),
		SourceImmutableVersion: claim.ImmutableVersion,
	}}}); err != nil {
		return err
	}
	for offset := 0; offset < len(responseData); offset += contentChunkBytes {
		end := min(offset+contentChunkBytes, len(responseData))
		if err := stream.Send(&runtimev1.GetInputResponseV1{Part: &runtimev1.GetInputResponseV1_Chunk{Chunk: responseData[offset:end]}}); err != nil {
			return err
		}
	}
	return nil
}

// Record fixed failure fields. Content, credentials, and error text stay private.
func (s *ContentServer) logInputFailure(ctx context.Context, claim ContentClaim, boundary string, code codes.Code, err error) {
	s.logger.ErrorContext(ctx, "runtime input content failed",
		"execution_id", claim.ExecutionID,
		"generation", claim.Generation,
		"boundary", boundary,
		"code", code.String(),
		"stage", runtimeContextUnavailableStage(err),
	)
}

// GetEliteaClientToken serves the Elitea client-token runtime context of the
// claimed execution.
func (s *ContentServer) GetEliteaClientToken(ctx context.Context, request *runtimev1.GetEliteaClientTokenRequestV1) (*runtimev1.GetEliteaClientTokenResponseV1, error) {
	if s.runtimeToken == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	// A governed run's toolkit credentials come only from RedeemCredential
	// (#1118), so its response carries no secrets-header value. The token
	// itself stays: the worker uses it for the model path, which ADR-0039
	// does not change. A deployment that never ran governed reads no pin.
	governed, err := s.governedRun(ctx, claim.ExecutionID)
	if err != nil {
		s.logger.WarnContext(ctx, "runtime context unavailable", "stage", "credential_profile_pin")
		return nil, contentStatus(codes.Unavailable)
	}
	// The worker gets only the optional fields it said it can read: both
	// workers refuse a response that carries a key they do not know (the
	// native worker parses with `deny_unknown_fields`), so a field served
	// unconditionally would fail every redemption on a pod a rolling deploy
	// has not replaced yet. Silence means "no"; an unknown name is ignored.
	value, err := s.runtimeToken.ResolveAccepting(ctx, claim, RuntimeContextAccept{
		SecretsHeaderValue: !governed && acceptsField(request.GetAcceptFields(), runtimeContextFieldSecretsHeader),
		Workspace:          acceptsField(request.GetAcceptFields(), workspaceruntime.RuntimeContextField),
		GovernedRun:        governed,
	})
	if err != nil {
		code := codes.Unavailable
		if errors.Is(err, ErrContentUnauthorized) {
			code = codes.PermissionDenied
		} else if errors.Is(err, ErrContentUnavailable) {
			s.logger.WarnContext(ctx, "runtime context unavailable", "stage", runtimeContextUnavailableStage(err))
		}
		return nil, contentStatus(code)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeContextResponseBytes {
		clearContentBytes(encoded)
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.GetEliteaClientTokenResponseV1{ContextJson: encoded}, nil
}

// governedRun reports whether the execution is pinned to the governed
// credential profile. Without a pin reader it reads nothing.
func (s *ContentServer) governedRun(ctx context.Context, executionID string) (bool, error) {
	if s.governedPins == nil {
		return false, nil
	}
	pin, err := s.governedPins.ReadPin(ctx, executionID)
	if errors.Is(err, profile.ErrPinNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pin.Profile == profile.ProfileGoverned, nil
}

// GetApplicationVersion serves one frozen nested (agent-as-tool) child
// definition under the live claim that already authorized the parent turn.
// It authorizes exactly as GetEliteaClientToken does: one channel, one
// authority.
func (s *ContentServer) GetApplicationVersion(ctx context.Context, request *runtimev1.GetApplicationVersionRequestV1) (*runtimev1.GetApplicationVersionResponseV1, error) {
	if s.runtimeVersions == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	if request.GetApplicationId() == 0 || request.GetVersionId() == 0 {
		return nil, contentStatus(codes.InvalidArgument)
	}
	value, err := s.runtimeVersions.Resolve(ctx, claim, request.GetApplicationId(), request.GetVersionId())
	if err != nil {
		code := codes.Unavailable
		switch {
		case errors.Is(err, ErrContentUnauthorized):
			code = codes.PermissionDenied
		case errors.Is(err, ErrContentNotFound):
			// The claim was good and the agent/version pair was not. Kept
			// distinct from PermissionDenied so an operator can tell a stale
			// nested reference from a rejected claim.
			code = codes.NotFound
		case errors.Is(err, ErrContentUnavailable):
			s.logger.WarnContext(ctx, "nested application version unavailable", "stage", runtimeContextUnavailableStage(err))
		}
		return nil, contentStatus(code)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeApplicationVersionResponseBytes {
		// Refused, never trimmed: the body is one indivisible definition, and a
		// truncated one would parse into an agent missing tools its author
		// attached.
		clearContentBytes(encoded)
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.GetApplicationVersionResponseV1{VersionJson: encoded}, nil
}

// GetAttachmentObject serves one stored chat attachment's text under the live
// claim that already authorized the turn the file rides on.
//
// A stored object with no text this RPC can serve — encrypted, an image, an
// unsupported or malformed format, over the input limit, or without a text
// layer — is neither a rejected claim nor a missing file: it is a successful
// call that names the reason (elitea.runtime.attachment-unreadable.v1), which
// the worker maps to a terminal "unreadable" outcome and tells the model.
func (s *ContentServer) GetAttachmentObject(ctx context.Context, request *runtimev1.GetAttachmentObjectRequestV1) (*runtimev1.GetAttachmentObjectResponseV1, error) {
	if s.runtimeObjects == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	// The attachment RPC's OWN pool: one read can wait for an extraction, so
	// in the shared pool a burst of first-time PDFs would refuse every token,
	// version and artifact call on the listener.
	release, err := acquireContentSlot(s.attachmentRequests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	if !boundedClaimPart(request.GetBucket()) || !boundedClaimPart(request.GetName()) {
		return nil, contentStatus(codes.InvalidArgument)
	}
	value, err := s.runtimeObjects.Resolve(ctx, claim, request.GetBucket(), request.GetName())
	if err != nil {
		code := codes.Unavailable
		switch {
		case errors.Is(err, ErrContentUnauthorized):
			// Also the CROSS-CONVERSATION refusal: deliberately the same
			// refusal a stale or foreign claim gets, so a caller cannot learn
			// that an object exists in a conversation it holds no claim for.
			code = codes.PermissionDenied
		case errors.Is(err, ErrContentNotFound):
			code = codes.NotFound
		case errors.Is(err, ErrContentRejected):
			reason := string(ReasonAttachmentUnreadable)
			var refusal *AttachmentUnreadableError
			if errors.As(err, &refusal) && refusal.Reason != "" {
				reason = string(refusal.Reason)
			}
			return &runtimev1.GetAttachmentObjectResponseV1{Outcome: &runtimev1.GetAttachmentObjectResponseV1_UnreadableReason{UnreadableReason: reason}}, nil
		case errors.Is(err, ErrContentUnavailable):
			s.logger.WarnContext(ctx, "attachment object unavailable", "stage", runtimeContextUnavailableStage(err))
		}
		return nil, contentStatus(code)
	}
	encoded, err := encodeAttachmentDocument(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeAttachmentObjectResponseBytes {
		// Refused, never trimmed. A truncated document is worse than an
		// unread one: the model is shown a prefix and told it is the file.
		clearContentBytes(encoded)
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.GetAttachmentObjectResponseV1{Outcome: &runtimev1.GetAttachmentObjectResponseV1_DocumentJson{DocumentJson: encoded}}, nil
}

// encodeAttachmentDocument encodes without HTML escaping. The default
// encoder writes `<`, `>` and `&` as six-byte escapes, which would let a text
// full of markup grow six-fold and miss the response ceiling the text limit
// was sized for. The body is JSON for the worker, never HTML.
func encodeAttachmentDocument(value any) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buffer.String(), "\n")), nil
}

// WriteSkill creates or updates one Skill from the conversation the claim
// authorizes (#940 A8, `skills_builder`). It keeps every property the reads
// have — same claim, same concurrency gate, same error taxonomy — and adds a
// bounded request document, refused rather than truncated.
func (s *ContentServer) WriteSkill(ctx context.Context, request *runtimev1.WriteSkillRequestV1) (*runtimev1.WriteSkillResponseV1, error) {
	var document RuntimeSkillWriteRequest
	claim, release, err := s.builderRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeBuilders.WriteSkill(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "skill write unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeBuilderResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.WriteSkillResponseV1{ResponseJson: encoded}, nil
}

// WriteProjectContext writes the claimed project's Project Context (#940 A8,
// `project_context_builder`). Twin of WriteSkill.
func (s *ContentServer) WriteProjectContext(ctx context.Context, request *runtimev1.WriteProjectContextRequestV1) (*runtimev1.WriteProjectContextResponseV1, error) {
	var document RuntimeProjectContextWriteRequest
	claim, release, err := s.builderRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeBuilders.WriteProjectContext(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "project context write unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeBuilderResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.WriteProjectContextResponseV1{ResponseJson: encoded}, nil
}

func (s *ContentServer) builderRequest(ctx context.Context, scope *runtimev1.RuntimeContentScopeV1, raw []byte, target any) (ContentClaim, func(), error) {
	if s.runtimeBuilders == nil {
		return ContentClaim{}, nil, contentStatus(codes.Unimplemented)
	}
	return s.claimedDocument(ctx, scope, raw, target, maxRuntimeBuilderRequestBytes)
}

// ListArtifacts, ReadArtifact, WriteArtifact and DeleteArtifact are the
// `artifact` toolkit family's four operations (#906). They are twins of
// WriteSkill in every respect that authorizes anything and differ only in the
// document each carries.
func (s *ContentServer) ListArtifacts(ctx context.Context, request *runtimev1.ListArtifactsRequestV1) (*runtimev1.ListArtifactsResponseV1, error) {
	var document RuntimeArtifactListRequest
	claim, release, err := s.artifactRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeArtifacts.List(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "artifact listing unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeArtifactResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.ListArtifactsResponseV1{ResponseJson: encoded}, nil
}

func (s *ContentServer) ReadArtifact(ctx context.Context, request *runtimev1.ReadArtifactRequestV1) (*runtimev1.ReadArtifactResponseV1, error) {
	var document RuntimeArtifactReadRequest
	claim, release, err := s.artifactRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeArtifacts.Read(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "artifact read unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeArtifactResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.ReadArtifactResponseV1{ResponseJson: encoded}, nil
}

func (s *ContentServer) WriteArtifact(ctx context.Context, request *runtimev1.WriteArtifactRequestV1) (*runtimev1.WriteArtifactResponseV1, error) {
	var document RuntimeArtifactWriteRequest
	claim, release, err := s.artifactRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeArtifacts.Write(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "artifact write unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeArtifactResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.WriteArtifactResponseV1{ResponseJson: encoded}, nil
}

func (s *ContentServer) DeleteArtifact(ctx context.Context, request *runtimev1.DeleteArtifactRequestV1) (*runtimev1.DeleteArtifactResponseV1, error) {
	var document RuntimeArtifactDeleteRequest
	claim, release, err := s.artifactRequest(ctx, request.GetScope(), request.GetRequestJson(), &document)
	if err != nil {
		return nil, err
	}
	defer release()
	value, err := s.runtimeArtifacts.Delete(ctx, claim, document)
	if err != nil {
		return nil, s.builderError(ctx, err, "artifact delete unavailable")
	}
	encoded, err := runtimeResponse(value, maxRuntimeArtifactResponseBytes)
	if err != nil {
		return nil, err
	}
	return &runtimev1.DeleteArtifactResponseV1{ResponseJson: encoded}, nil
}

// artifactRequest is the claim-and-document half all four artifact RPCs
// share: four copies would be four places for the authorization to drift.
func (s *ContentServer) artifactRequest(ctx context.Context, scope *runtimev1.RuntimeContentScopeV1, raw []byte, target any) (ContentClaim, func(), error) {
	if s.runtimeArtifacts == nil {
		return ContentClaim{}, nil, contentStatus(codes.Unimplemented)
	}
	return s.claimedDocument(ctx, scope, raw, target, maxRuntimeArtifactRequestBytes)
}

// claimedDocument takes a shared-pool slot, parses the claim and decodes one
// bounded JSON document refusing unknown keys — the mirror of the worker's
// own `deny_unknown_fields`, so a version skew fails loudly instead of
// half-applying a write. The caller runs release when done.
func (s *ContentServer) claimedDocument(ctx context.Context, scope *runtimev1.RuntimeContentScopeV1, raw []byte, target any, maxBytes int64) (ContentClaim, func(), error) {
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return ContentClaim{}, nil, err
	}
	claim, err := parseExecutionClaim(ctx, scope)
	if err != nil {
		release()
		return ContentClaim{}, nil, contentStatus(codes.Unauthenticated)
	}
	if int64(len(raw)) > maxBytes {
		release()
		return ContentClaim{}, nil, contentStatus(codes.ResourceExhausted)
	}
	if err := decodeStrictDocument(raw, target); err != nil {
		release()
		return ContentClaim{}, nil, contentStatus(codes.InvalidArgument)
	}
	return claim, release, nil
}

// decodeStrictDocument decodes exactly one JSON value, refusing unknown keys
// and trailing data.
func decodeStrictDocument(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the document")
	}
	return nil
}

// builderError maps one builder or artifact failure onto the shared
// taxonomy. FailedPrecondition (ErrContentRejected) means the document the
// model composed cannot be stored as written, which the model can read and
// retry from — the worker does not fail the turn on it.
func (s *ContentServer) builderError(ctx context.Context, err error, message string) error {
	code := codes.Unavailable
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		code = codes.PermissionDenied
	case errors.Is(err, ErrContentNotFound):
		code = codes.NotFound
	case errors.Is(err, ErrContentRejected):
		code = codes.FailedPrecondition
	case errors.Is(err, ErrContentUnavailable):
		s.logger.WarnContext(ctx, message, "stage", runtimeContextUnavailableStage(err))
	}
	return contentStatus(code)
}

// runtimeResponse encodes one indivisible response document. An over-cap
// document is REFUSED rather than trimmed: a truncated one either fails the
// client's decode or parses into something that looks complete.
func runtimeResponse(value any, maxBytes int) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxBytes {
		return nil, contentStatus(codes.Unavailable)
	}
	return encoded, nil
}

// acquireContentSlot takes one slot of a fixed pool without queueing: a full
// pool is Unavailable, which every client retries.
func acquireContentSlot(slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, grpcstatus.Error(codes.Unavailable, "The runtime content service is busy. Retry.")
	}
}

// contentStatus is the content plane's refusal: a fixed message per code,
// never the error text (which may name storage paths, claims or tenants).
func contentStatus(code codes.Code) error {
	message := "The runtime content request failed."
	switch code {
	case codes.InvalidArgument:
		message = "The runtime content request is malformed."
	case codes.Unauthenticated:
		message = "A verified workload and a well-formed claim are required."
	case codes.PermissionDenied:
		message = "The claim or grant is not accepted."
	case codes.NotFound:
		message = "The requested content does not exist."
	case codes.Aborted:
		message = "The request conflicts with the durable execution state."
	case codes.ResourceExhausted:
		message = "The content exceeds its limit."
	case codes.FailedPrecondition:
		message = "The content cannot be served or stored as written."
	case codes.DeadlineExceeded:
		message = "The content transfer was interrupted. Retry."
	case codes.Unavailable:
		message = "The runtime content service is unavailable. Retry."
	case codes.Unimplemented:
		message = "This deployment does not serve this content."
	}
	return grpcstatus.Error(code, message)
}

func clearContentBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// contentPeer is the verified TLS state of the call: the listener requires a
// runtime-CA client certificate at the handshake, so a call without one never
// reaches a handler, and this refuses one that somehow does.
func contentPeer(ctx context.Context) (*tls.ConnectionState, bool) {
	info, ok := peer.FromContext(ctx)
	if !ok || info == nil {
		return nil, false
	}
	var state tls.ConnectionState
	switch auth := info.AuthInfo.(type) {
	case credentials.TLSInfo:
		state = auth.State
	case *credentials.TLSInfo:
		if auth == nil {
			return nil, false
		}
		state = auth.State
	default:
		return nil, false
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 || len(state.PeerCertificates) == 0 {
		return nil, false
	}
	return &state, true
}

func parseContentClaim(ctx context.Context, scope *runtimev1.RuntimeContentScopeV1, contentID, version string) (ContentClaim, error) {
	claim, err := parseExecutionClaim(ctx, scope)
	if err != nil {
		return ContentClaim{}, err
	}
	if !boundedClaimPart(contentID) || !boundedClaimPart(version) {
		return ContentClaim{}, fmt.Errorf("%w: incomplete claim", ErrContentUnauthorized)
	}
	claim.ContentID = contentID
	claim.ImmutableVersion = version
	return claim, nil
}

// parseExecutionClaim binds the verified peer certificate, the scope's
// execution and generation, and the claim id and fence of the call's metadata
// (grpcauth.ContentClaim) into one claim. It authorizes nothing by itself:
// every consumer checks the claim against the durable execution state.
func parseExecutionClaim(ctx context.Context, scope *runtimev1.RuntimeContentScopeV1) (ContentClaim, error) {
	state, ok := contentPeer(ctx)
	if !ok {
		return ContentClaim{}, ErrContentUnauthorized
	}
	generation := scope.GetGeneration()
	if generation == 0 || generation > maxRuntimeGeneration {
		return ContentClaim{}, ErrContentUnauthorized
	}
	executionID := scope.GetExecutionId()
	if !boundedClaimPart(executionID) {
		return ContentClaim{}, fmt.Errorf("%w: incomplete claim", ErrContentUnauthorized)
	}
	claimID, fence, err := grpcauth.ContentClaim(ctx)
	if err != nil || len(fence) != sha256.Size || !boundedClaimPart(claimID) {
		return ContentClaim{}, ErrContentUnauthorized
	}
	return ContentClaim{
		PeerCertificate: state.VerifiedChains[0][0],
		ExecutionID:     executionID,
		Generation:      generation,
		ClaimID:         claimID,
		FenceToken:      fence,
	}, nil
}

// acceptsField reports whether the call asked for one optional
// runtime-context field by name, case-blind and trimmed.
// acceptsField reads accept_fields. Each repeated entry is itself split on
// commas, so a caller that joins names into one entry (the old HTTP accept
// header's shape) and one that sends each name separately are read alike.
func acceptsField(fields []string, field string) bool {
	for _, entry := range fields {
		for name := range strings.SplitSeq(entry, ",") {
			if strings.EqualFold(strings.TrimSpace(name), field) {
				return true
			}
		}
	}
	return false
}

func boundedClaimPart(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := range len(value) {
		switch value[index] {
		case '\r', '\n', 0:
			return false
		}
	}
	return true
}

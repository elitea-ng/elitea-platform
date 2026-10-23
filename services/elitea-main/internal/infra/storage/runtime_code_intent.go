package storage

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	runtime "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

type OriginalCodeIntentStore interface {
	RegisterOriginalCodeVisit(context.Context, ContentClaim, code.VisitRequest) (code.VisitResponse, error)
	RegisterOriginalCodeIntent(context.Context, ContentClaim, code.IntentRequest) (code.SignedGrant, error)
}

// CodeTransaction is for short publication/acquisition metadata phases only.
// No provider, binary or object-store IO may run while the callback owns row locks.
type CodeTransaction interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type OriginalCodeVisitConsumer interface {
	WithOriginalCodeVisit(context.Context, ContentClaim, code.OriginalVisitRef, string, func(context.Context, CodeTransaction, OriginalCodeVisit) error) error
}

// RegisteredCodeIntent is the once-frozen final binding. It contains no runtime
// identity; a retained runtime must be observed through the original Supervisor.
type RegisteredCodeIntent struct {
	Original            OriginalCodeVisit
	Binding             code.Binding
	BindingSHA256       string
	PreparedFingerprint string
	Compiled            bool
	CompiledExecute     *OriginalCompiledCodeExecute
	Access              CodeIntentAccess
}

// OriginalCompiledCodeExecute is read-only data from the registered original
// final selector/profile/index. It is not a caller-selected execution grant.
type OriginalCompiledCodeExecute struct {
	Binding                runtime.RustSnapshotBinding
	DescriptorSHA256       string
	SnapshotKeySHA256      string
	DependencyBundleSHA256 string
}
type CodeIntentAccess struct {
	ClaimID             string
	ClaimAttempt        uint64
	LeaseEpoch          uint64
	FenceSHA256         string
	WorkloadIdentity    string
	IssuedAtUnixMillis  int64
	ExpiresAtUnixMillis int64
}

// CodePlatformIntentConsumer resolves the whole request digest only from its registered
// original intent. The caller supplies the final prepared fingerprint, never a runtime.
type CodePlatformIntentConsumer interface {
	WithRegisteredCodePlatformIntent(context.Context, ContentClaim, string, string, func(context.Context, CodeTransaction, RegisteredCodeIntent) error) error
}

type RegisteredCodeIntentConsumer interface {
	WithRegisteredCodeIntent(context.Context, ContentClaim, string, string, string, func(context.Context, CodeTransaction, RegisteredCodeIntent) error) error
}

// OriginalCodeVisit contains immutable original facts; access claim identity is separate.
type OriginalCodeVisit struct {
	TenantID                string
	ResourceProjectID       int64
	ProjectionProjectID     int64
	ActorID                 int64
	CurrentClaimAttempt     uint64
	CurrentClaimID          string
	CurrentWorkloadIdentity string
	LeaseEpoch              uint64
	Reference               code.OriginalVisitRef
	ExecutionID             string
	OriginalGeneration      uint64
	ActivationID            string
	NodeID                  string
	GraphThread             string
	Step                    uint64
	Attempt                 uint16
	NodeDigest              string
	OwningYAMLSHA256        string
	// This is Main captured pre-redemption source identity, not a Rust compiler hash.
	OwningSourceDefinition     scope.SourceReference
	PreWorkspacePreparedSHA256 string
	SourceSHA256               string
	InputSHA256                string
	Language                   string
	PreWorkspaceFingerprint    string
	ImageDigest                string
	PolicyRevision             string
	TimeoutSeconds             uint32
	SavedChildScope            json.RawMessage
	Declaration                SavedCodeDeclaration
}

// CodePreparedMetadata contains only digests of actual admitted data.
type CodePreparedMetadata struct {
	Language       string `json:"language"`
	PreparedSHA256 string `json:"prepared_job_sha256"`
	SourceSHA256   string `json:"source_sha256"`
	InputSHA256    string `json:"input_sha256"`
	Fingerprint    string `json:"prepared_fingerprint"`
	ImageDigest    string `json:"image_digest"`
	PolicyRevision string `json:"policy_revision"`
	TimeoutSeconds uint32 `json:"timeout_seconds"`
}

func ValidateOriginalCodeVisit(r code.VisitRequest) ([]byte, []byte, error) {
	if r.Schema != "elitea.sandbox.original-code-visit-request.v1" || !code.NonzeroDigest(r.ActivationID) || !code.NonzeroDigest(r.NodeDigest) || !code.NonzeroDigest(r.OwningYAMLSHA256) || r.Step > math.MaxInt64 || r.Attempt < 1 || r.Attempt > 16 || len(r.SavedChildScope) == 0 {
		return nil, nil, code.ErrRejected
	}
	if !code.VisitBounds(r.ActivationID, r.NodeID, r.GraphThread, r.Step, r.Attempt) {
		return nil, nil, code.ErrRejected
	}
	configuration, err := code.DecodeBase64(r.ConfigurationBase64URL, 2*1024*1024)
	if err != nil || len(configuration) == 0 {
		return nil, nil, code.ErrRejected
	}
	prepared, err := code.DecodeBase64(r.PreWorkspacePreparedBase64URL, 1024*1024)
	if err != nil || len(prepared) == 0 {
		return nil, nil, code.ErrRejected
	}
	return configuration, prepared, nil
}
func ValidateOriginalCodeIntent(r code.IntentRequest) ([]byte, error) {
	if r.Schema != "elitea.sandbox.original-code-intent-request.v1" || r.OriginalVisit.Validate() != nil || !code.NonzeroDigest(r.DispatchActivation) || !code.NonzeroDigest(r.RequestDigest) || !code.Identity(r.SupervisorAudience) || (r.CompiledBindingBase64URL == nil) != (r.SelectedDescriptorSHA256 == nil) {
		return nil, code.ErrRejected
	}
	if r.SelectedDescriptorSHA256 != nil && !code.NonzeroDigest(*r.SelectedDescriptorSHA256) {
		return nil, code.ErrRejected
	}
	return code.DecodeBase64(r.PreparedBase64URL, 1024*1024)
}

// MatchCodePrepared compares saved Code semantics. The caller must also apply
// OriginalSavedCodeInputPolicy from the exact owning graph before persisting a visit.
// It never executes a source template.
func MatchCodePrepared(declaration SavedCodeDeclaration, configuration, prepared []byte) (CodePreparedMetadata, error) {
	if code.Digest(append([]byte("elitea.graph.code.config.v1\x00"), configuration...)) != hex.EncodeToString(declaration.ConfigurationDigest[:]) {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil || job.Revision != 1 || job.Workspace != nil || job.Broker != nil {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	return matchCodePreparedSemantics(declaration, job)
}
func matchCodePreparedSemantics(declaration SavedCodeDeclaration, job CodePreparedRequest) (CodePreparedMetadata, error) {
	if job.Language != declaration.Language {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	// Input membership is admitted against the exact owning graph policy by the
	// visit transaction. Final preparation preserves its retained input digest.
	var source struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if code.Decode(declaration.Source, &source, 2*1024*1024) != nil {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	if source.Type == "fixed" && source.Value != job.Source {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	if source.Type != "fixed" && source.Type != "variable" && source.Type != "fstring" {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	return CodePreparedMetadata{Language: job.Language, PreparedSHA256: job.PreparedSHA256, SourceSHA256: code.Digest([]byte(job.Source)), InputSHA256: code.Digest(job.Input), Fingerprint: job.Fingerprint, ImageDigest: job.ImageDigest, PolicyRevision: job.PolicyRevision, TimeoutSeconds: job.TimeoutSeconds}, nil
}

// MatchFinalCodePrepared preserves original source/input/profile across dependency preparation.
// The workspace owner validates final workspace acquisition/manifest under this same transaction.
func MatchFinalCodePrepared(declaration SavedCodeDeclaration, original CodePreparedMetadata, prepared []byte) (CodePreparedMetadata, error) {
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	metadata, err := matchCodePreparedSemantics(declaration, job)
	if err != nil || metadata.Language != original.Language || metadata.SourceSHA256 != original.SourceSHA256 || metadata.InputSHA256 != original.InputSHA256 || metadata.ImageDigest != original.ImageDigest || metadata.PolicyRevision != original.PolicyRevision || metadata.TimeoutSeconds != original.TimeoutSeconds {
		return CodePreparedMetadata{}, code.ErrRejected
	}
	return metadata, nil
}

func (s *ContentServer) WithOriginalCodeIntents(store OriginalCodeIntentStore) *ContentServer {
	if s != nil {
		s.originalCodeIntents = store
	}
	return s
}

// maxCodeRegistrationBytes bounds one visit or intent document.
const maxCodeRegistrationBytes = 5 * 1024 * 1024

// RegisterCodeVisit registers the original Code sandbox visit of the claimed
// execution.
func (s *ContentServer) RegisterCodeVisit(ctx context.Context, request *runtimev1.RegisterCodeVisitRequestV1) (*runtimev1.RegisterCodeVisitResponseV1, error) {
	if s.originalCodeIntents == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Code visit claim denied")
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	raw := request.GetVisitJson()
	invalid := grpcstatus.Error(codes.InvalidArgument, "Invalid Code visit")
	var visitRequest code.VisitRequest
	if len(raw) > maxCodeRegistrationBytes || code.Decode(raw, &visitRequest, maxCodeRegistrationBytes) != nil || !code.RequiredFields(raw, []string{"schema", "activation_id", "node_id", "graph_thread", "step", "attempt", "node_digest", "owning_yaml_sha256", "exact_configuration_json_base64url", "pre_workspace_prepared_job_json_base64url", "saved_child_scope"}, "saved_child_scope") {
		return nil, invalid
	}
	if _, _, err := ValidateOriginalCodeVisit(visitRequest); err != nil {
		return nil, invalid
	}
	visit, err := s.originalCodeIntents.RegisterOriginalCodeVisit(ctx, claim, visitRequest)
	if err != nil {
		return nil, originalCodeStatus(err)
	}
	encoded, err := nodeRecoveryJSON(visit)
	if err != nil {
		return nil, err
	}
	return &runtimev1.RegisterCodeVisitResponseV1{VisitJson: encoded}, nil
}

// RegisterCodeIntent registers the original whole-Code intent of a visit and
// answers Main's signed intent grant.
func (s *ContentServer) RegisterCodeIntent(ctx context.Context, request *runtimev1.RegisterCodeIntentRequestV1) (*runtimev1.RegisterCodeIntentResponseV1, error) {
	if s.originalCodeIntents == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Code intent claim denied")
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	raw := request.GetIntentJson()
	invalid := grpcstatus.Error(codes.InvalidArgument, "Invalid Code intent")
	var intentRequest code.IntentRequest
	if len(raw) > maxCodeRegistrationBytes || code.Decode(raw, &intentRequest, maxCodeRegistrationBytes) != nil || !code.RequiredFields(raw, []string{"schema", "original_visit", "dispatch_activation", "request_digest", "supervisor_audience", "prepared_job_json_base64url", "compiled_binding_json_base64url", "selected_descriptor_sha256"}, "compiled_binding_json_base64url", "selected_descriptor_sha256") {
		return nil, invalid
	}
	if _, err := ValidateOriginalCodeIntent(intentRequest); err != nil {
		return nil, invalid
	}
	intent, err := s.originalCodeIntents.RegisterOriginalCodeIntent(ctx, claim, intentRequest)
	if err != nil {
		return nil, originalCodeStatus(err)
	}
	encoded, err := nodeRecoveryJSON(code.IntentResponse{Schema: "elitea.sandbox.original-code-intent-response.v1", Intent: intent})
	if err != nil {
		return nil, err
	}
	return &runtimev1.RegisterCodeIntentResponseV1{ResponseJson: encoded}, nil
}

func originalCodeStatus(err error) error {
	if errors.Is(err, code.ErrRejected) {
		return grpcstatus.Error(codes.Aborted, "Code authority denied")
	}
	return nodeRecoveryControlStatus(err)
}

type OriginalCodeWorkspaceVerifier interface {
	VerifyOriginalCodeWorkspace(context.Context, CodeTransaction, ContentClaim, OriginalCodeVisit, CodePreparedRequest) error
}
type OriginalCodeBrokerVerifier interface {
	VerifyOriginalCodeBroker(context.Context, CodeTransaction, ContentClaim, OriginalCodeVisit, CodePreparedRequest) error
}

type CodeCompileVisitAccess struct {
	OriginalVisit       code.OriginalVisitRef
	ClaimID             string
	ClaimAttempt        uint64
	LeaseEpoch          uint64
	FenceSHA256         []byte
	ExpiresAtUnixMillis int64
}
type OriginalCodeWorkspaceCompileAuthorizer interface {
	AuthorizeCodeWorkspaceCompile(context.Context, *x509.Certificate, runtime.Fence, code.OriginalVisitRef, []byte) (CodeCompileVisitAccess, error)
}

// MatchCodeSnapshotPrepared validates exact rev1..5 bytes through the shared strict parser.
// It preserves the existing base fingerprint/profile and source/image/policy pins.
func MatchCodeSnapshotPrepared(binding runtime.RustSnapshotBinding, raw []byte) (string, error) {
	job, err := ParseCodePreparedRequest(raw)
	if err != nil || binding.Validate() != nil || job.Language != "rust" || job.Fingerprint != binding.BasePreparedRequestSHA256 || job.Source == "" || code.Digest([]byte(job.Source)) != binding.SourceSHA256 || job.ImageDigest != binding.ExecutionImageDigest || job.PolicyRevision != binding.PolicyRevision {
		return "", code.ErrRejected
	}
	if job.Broker != nil && binding.PolicyRevision != "cargo-broker-execute-v1" {
		return "", code.ErrRejected
	}
	return job.DependencyBundleSHA256, nil
}

// CodeBrokerEffectFacts are observations from the exact registered call journal.
// They cannot prove whole-Code completion or no effect by themselves.
type CodeBrokerEffectFacts struct {
	Registered                bool
	HasObservedCalls          bool
	HasDispatchedEffects      bool
	HasUncertainEffects       bool
	HasPendingToolkitChildren bool
}
type OriginalCodeBrokerEffectFactsReader interface {
	ReadOriginalCodeBrokerEffects(context.Context, CodeTransaction, string, uint64, string, string, string) (CodeBrokerEffectFacts, error)
}

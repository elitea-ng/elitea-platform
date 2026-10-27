package credentialbroker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	// MaxEnvelopeTTL is the policy maximum lifetime of released material:
	// an envelope's not_after is never later than redemption + this. Policy
	// schema v1 has no field for it.
	MaxEnvelopeTTL = 5 * time.Minute
	// maxSessionTTL bounds the run-scoped agent session. The agent deadline
	// of a run is 24 hours.
	maxSessionTTL = 24 * time.Hour
	// WorkloadScope is the SecretStore workload scope of every broker read:
	// with Vault/OpenBao the broker logs in as role
	// <ELITEA_SECRETSTORE_VAULT_ROLE_PREFIX>-credential-broker.
	WorkloadScope = "credential-broker"
	// assertedIssuer names Elitea as the issuer of asserted user evidence.
	assertedIssuer = "elitea"
)

// supportedObligations are the obligations this enforcement point fulfils.
// The decision is appended to the audit outbox synchronously, in the
// redemption's transaction, so audit.sync is fulfilled; an approval step is
// not, so a grant that requires one denies.
var supportedObligations = map[string]bool{credentialgrant.ObligationAuditSync: true}

// Decider is the AND-composed operation decision (*authzcompose.Decider).
type Decider interface {
	Decide(ctx context.Context, r authzcompose.OperationRequest) authzcompose.OperationDecision
}

// SessionOpener opens the run-scoped agent session (*agentidentity.Broker).
type SessionOpener interface {
	OpenSession(ctx context.Context, in agentidentity.SessionAdmission) (agentidentity.SessionRef, error)
}

// DelegationCustody opens a delegation's assertion for an on-behalf-of
// exchange and records a credential authorization requirement
// (*delegationstore.Store).
type DelegationCustody interface {
	Load(ctx context.Context, r delegationstore.LoadRequest) (obo.Assertion, delegationstore.Delegation, error)
	RecordRequirement(ctx context.Context, r delegationstore.Requirement) (delegationstore.Requirement, error)
}

// Store persists invocations, decisions and redemptions.
type Store interface {
	// AdmitInvocation inserts the invocation, or matches a replay of it
	// (ErrInvocationMismatch when a field differs), then calls decide and
	// appends the event it returns, all in one transaction. An append
	// failure wraps ErrAuditFailed and rolls the invocation back.
	AdmitInvocation(ctx context.Context, run ClaimedRun, invocation BoundToolInvocation,
		decide func(context.Context) (securityaudit.SecurityEvent, error)) error
	// RecordRedemption inserts the redemption row and appends its event in
	// one transaction. An append failure wraps ErrAuditFailed.
	RecordRedemption(ctx context.Context, run ClaimedRun, redemption Redemption, event securityaudit.SecurityEvent) error
	// AgentSession answers the session linked to (execution, generation).
	AgentSession(ctx context.Context, executionID string, generation uint64) (string, bool, error)
	// LinkAgentSession links ref unless a session is linked already, and
	// answers the linked one.
	LinkAgentSession(ctx context.Context, executionID string, generation uint64, ref string) (string, error)
	// SessionCeiling answers the audiences the agent binding's active grants
	// target in the tenant project: the ceiling of the run's session.
	SessionCeiling(ctx context.Context, tenantProjectID int64, agentBindingID string) ([]string, error)
}

// Redemption is the stored record of one release. It holds no material.
type Redemption struct {
	RedemptionID string
	InvocationID string
	DecisionID   string
	Kind         EnvelopeKind
	NotAfter     time.Time
}

// Config is the broker's dependencies. Exchanger, Custody, Meter, Logger
// and Now are optional; every other field is required.
type Config struct {
	Decider     Decider
	References  credentialgrant.ReferenceReader
	Targets     credentialgrant.TargetReader
	Toolkits    credentialgrant.ToolkitBindingReader
	Bindings    authzcompose.BindingGetter
	Mandates    mandate.Reader
	Delegations authzcompose.DelegationReader
	// SecretStores are keyed by backend id (secretstore.BackendPGVault,
	// secretstore.BackendVault).
	SecretStores map[string]secretstore.SecretStore
	// Exchanger is the Entra on-behalf-of exchanger; Custody the delegation
	// custody it reads assertions from. Both or neither: without them an
	// oauth_delegated credential is BACKEND_UNAVAILABLE.
	Exchanger obo.Exchanger
	Custody   DelegationCustody
	// Store wraps the security audit appender: the decision and the
	// redemption events are appended in the store's transactions.
	Store    Store
	Pins     profile.PinReader
	Sessions SessionOpener
	Catalog  OperationCatalog
	Meter    metric.Meter
	Logger   *slog.Logger
	Now      func() time.Time
}

// Broker redeems governed credentials. It is built once at the composition
// root, only with credential governance on.
type Broker struct {
	cfg     Config
	metrics metrics
	logger  *slog.Logger
	now     func() time.Time
	newID   func() (uuid.UUID, error)
}

// New builds the broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Decider == nil || cfg.References == nil || cfg.Targets == nil || cfg.Toolkits == nil ||
		cfg.Bindings == nil || cfg.Mandates == nil || cfg.Delegations == nil || cfg.Store == nil ||
		cfg.Pins == nil || cfg.Sessions == nil || cfg.Catalog == nil {
		return nil, errors.New("credential broker: decider, readers, store, pins, sessions and catalogue are required")
	}
	if len(cfg.SecretStores) == 0 {
		return nil, errors.New("credential broker: at least one secret store is required")
	}
	for backend, store := range cfg.SecretStores {
		if store == nil {
			return nil, fmt.Errorf("credential broker: secret store %q is nil", backend)
		}
	}
	if (cfg.Exchanger == nil) != (cfg.Custody == nil) {
		return nil, errors.New("credential broker: the on-behalf-of exchanger and the delegation custody come together")
	}
	m, err := newMetrics(cfg.Meter)
	if err != nil {
		return nil, fmt.Errorf("credential broker: metrics: %w", err)
	}
	b := &Broker{cfg: cfg, metrics: m, logger: cfg.Logger, now: cfg.Now, newID: uuid.NewV7}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b, nil
}

// Redeem runs one redemption. A refusal is a Result with a Denial; an error
// is a malformed request (ErrInvalidRequest) or an infrastructure failure,
// which the content server answers with a gRPC status.
func (b *Broker) Redeem(ctx context.Context, req RedeemRequest) (result Result, err error) {
	start := time.Now()
	defer func() {
		b.metrics.record(ctx, result, err, time.Since(start))
		if result.Denial != nil {
			// Codes and references only; never material or a request field.
			b.logger.InfoContext(ctx, "governed credential redemption denied",
				"execution_id", req.Run.ExecutionID, "generation", req.Run.Generation,
				"code", string(result.Denial.Code), "reason", result.Denial.reason,
				"decision_id", result.Denial.DecisionID)
		}
	}()
	if err := req.Invocation.validate(); err != nil {
		return Result{}, err
	}
	if req.Run.ExecutionID == "" || req.Run.Generation == 0 || req.Run.TenantProjectID <= 0 || req.Run.WorkloadID == "" {
		return Result{}, ErrInvalidRequest
	}

	// 1. The run's pin must be governed.
	pin, err := b.cfg.Pins.ReadPin(ctx, req.Run.ExecutionID)
	if errors.Is(err, profile.ErrPinNotFound) {
		return deny(CodeNotGoverned, "not_governed", ""), nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("credential broker: read the run's pin: %w", err)
	}
	if pin.Profile != profile.ProfileGoverned {
		return deny(CodeNotGoverned, "not_governed", ""), nil
	}

	// 2. The worker's context digest must be the stored context's.
	sc, ok := verifiedContext(req, pin)
	if !ok {
		return deny(CodeContextMismatch, "context_mismatch", ""), nil
	}

	// 3-5. Invocation row, decision and its audit event in one transaction.
	var (
		decision authzcompose.OperationDecision
		facts    requestFacts
	)
	err = b.cfg.Store.AdmitInvocation(ctx, req.Run, req.Invocation, func(ctx context.Context) (securityaudit.SecurityEvent, error) {
		var buildErr error
		facts, buildErr = b.buildRequest(ctx, sc, req)
		if buildErr != nil {
			return securityaudit.SecurityEvent{}, buildErr
		}
		decision = b.cfg.Decider.Decide(ctx, facts.request)
		return b.decisionEvent(req, facts, decision), nil
	})
	switch {
	case errors.Is(err, ErrInvocationMismatch):
		return deny(CodeInvocationMismatch, "invocation_mismatch", ""), nil
	case errors.Is(err, ErrAuditFailed):
		return deny(CodeDenied, "audit_failed", ""), nil
	case err != nil:
		return Result{}, fmt.Errorf("credential broker: admit the invocation: %w", err)
	}
	if !decision.Permitted() {
		reason := "denied"
		if len(decision.Reasons) > 0 {
			reason = decision.Reasons[0].AuditCode()
		}
		return deny(CodeDenied, reason, decision.DecisionID), nil
	}
	if err := authzcompose.Enforce(decision, supportedObligations); err != nil {
		return deny(CodeDenied, "obligation_unsupported", decision.DecisionID), nil
	}

	// 6. The run-scoped agent session, opened at the first redemption.
	if denial := b.ensureSession(ctx, sc, req, facts); denial != nil {
		denial.DecisionID = decision.DecisionID
		return Result{Denial: denial}, nil
	}

	// 6-7. Material by class, filtered to the catalogue's keys.
	material, denial := b.acquire(ctx, req, facts, decision)
	if denial != nil {
		denial.DecisionID = decision.DecisionID
		return Result{Denial: denial}, nil
	}
	envelope := material.envelope
	id, err := b.newID()
	if err != nil {
		envelope.Destroy()
		return Result{}, fmt.Errorf("credential broker: redemption id: %w", err)
	}
	envelope.RedemptionID, envelope.DecisionID = id.String(), decision.DecisionID
	envelope.TargetIdentity = facts.target.TargetIdentity
	for _, obligation := range decision.Obligations {
		envelope.Obligations = append(envelope.Obligations, obligation.Kind)
	}
	redemption := Redemption{
		RedemptionID: envelope.RedemptionID, InvocationID: req.Invocation.InvocationID,
		DecisionID: decision.DecisionID, Kind: envelope.Kind, NotAfter: envelope.NotAfter,
	}
	if err := b.cfg.Store.RecordRedemption(ctx, req.Run, redemption, b.redemptionEvent(req, facts, redemption)); err != nil {
		envelope.Destroy()
		if errors.Is(err, ErrAuditFailed) {
			return deny(CodeDenied, "audit_failed", decision.DecisionID), nil
		}
		return Result{}, fmt.Errorf("credential broker: record the redemption: %w", err)
	}
	return Result{Envelope: envelope}, nil
}

// verifiedContext checks the stored context against the request digest and
// the pin, and decodes it strictly.
func verifiedContext(req RedeemRequest, pin profile.Pin) (*securitycontextView, bool) {
	if len(req.Run.ContextBytes) == 0 || req.Run.ContextDigest == ([32]byte{}) ||
		req.SecurityContextDigest != req.Run.ContextDigest {
		return nil, false
	}
	message, err := securitycontext.Verify(req.Run.ContextBytes, securitycontext.Reference{Digest: runtimedomain.Digest(req.Run.ContextDigest)})
	if err != nil {
		return nil, false
	}
	view := viewOf(message)
	if view.policyRevisionID != pin.PolicyRevisionID || view.policyDigest != pin.PolicyDigest ||
		view.tenantProjectID != req.Run.TenantProjectID {
		return nil, false
	}
	return view, true
}

// ensureSession reuses the run's linked agent session or opens one. The
// session is opened only after a permit, so a refused use opens nothing.
func (b *Broker) ensureSession(ctx context.Context, sc *securitycontextView, req RedeemRequest, facts requestFacts) *Denial {
	if _, ok, err := b.cfg.Store.AgentSession(ctx, req.Run.ExecutionID, req.Run.Generation); err != nil {
		return &Denial{Code: CodeBackendUnavailable, reason: "agent_session_read"}
	} else if ok {
		return nil
	}
	ceiling, err := b.cfg.Store.SessionCeiling(ctx, sc.tenantProjectID, sc.agentBindingID)
	if err != nil {
		return &Denial{Code: CodeBackendUnavailable, reason: "agent_session_read"}
	}
	if len(ceiling) == 0 {
		return &Denial{Code: CodeDenied, reason: "agent_session_no_ceiling"}
	}
	admission := agentidentity.SessionAdmission{
		TenantProjectID: sc.tenantProjectID, ExecutionID: req.Run.ExecutionID, Generation: int64(req.Run.Generation),
		AgentBindingID: sc.agentBindingID, AgentBindingEpoch: sc.agentBindingEpoch,
		Ceiling: agentidentity.Narrowing{Audiences: ceiling}, PolicyRevisionID: sc.policyRevisionID,
	}
	expires := b.now().Add(maxSessionTTL)
	switch {
	case facts.delegation != nil:
		d := facts.delegation
		admission.Profile, admission.DelegationID = agentidentity.ProfileDelegatedVerified, d.ID
		audience := ""
		if len(d.Audiences) > 0 {
			audience = d.Audiences[0]
		}
		admission.Evidence = agentidentity.VerifiedUserEvidence{
			Issuer: d.Issuer, TenantID: d.TenantID, ObjectID: d.SubjectObjectID, Audience: audience, DelegationID: d.ID,
		}
		if d.ExpiresAt.Before(expires) {
			expires = d.ExpiresAt
		}
	case facts.mandate != nil:
		m := facts.mandate
		admission.Profile, admission.MandateID = agentidentity.ProfileAutonomousAttributed, m.ID
		if m.Kind == mandate.KindDelegatedBackground && facts.request.Subject.UserID > 0 {
			admission.Evidence = agentidentity.AssertedUserEvidence{
				Issuer: assertedIssuer, Subject: facts.request.Subject.PrincipalRef, MandateID: m.ID,
			}
		}
		if m.ExpiresAt.Before(expires) {
			expires = m.ExpiresAt
		}
	default:
		return &Denial{Code: CodeDenied, reason: "agent_session_no_authority"}
	}
	admission.ExpiresAt = expires
	ref, err := b.cfg.Sessions.OpenSession(ctx, admission)
	if err != nil {
		reason := "agent_session"
		if code, ok := agentidentity.ReasonOf(err); ok {
			reason = "agent_session." + string(code)
		}
		return &Denial{Code: CodeDenied, reason: reason}
	}
	if _, err := b.cfg.Store.LinkAgentSession(ctx, req.Run.ExecutionID, req.Run.Generation, string(ref)); err != nil {
		return &Denial{Code: CodeBackendUnavailable, reason: "agent_session_link"}
	}
	return nil
}

func (b *Broker) decisionEvent(req RedeemRequest, facts requestFacts, decision authzcompose.OperationDecision) securityaudit.SecurityEvent {
	event := b.baseEvent(securityaudit.TypeDecision, "credential.use", req, facts)
	event.DecisionID = decision.DecisionID
	event.Effect = securityaudit.EffectDeny
	if decision.Permitted() {
		event.Effect = securityaudit.EffectAllow
	}
	for _, reason := range decision.Reasons {
		if len(event.Reasons) == 32 {
			break
		}
		event.Reasons = append(event.Reasons, reason.AuditCode())
	}
	event.Arrangement = securityaudit.Arrangement(decision.Arrangement)
	event.AuthorizationEpoch = decision.EvaluatedEpochs["agent_binding"]
	return event
}

func (b *Broker) redemptionEvent(req RedeemRequest, facts requestFacts, redemption Redemption) securityaudit.SecurityEvent {
	event := b.baseEvent(securityaudit.TypeCredentialRedeem, "credential.redeem", req, facts)
	event.DecisionID = redemption.DecisionID
	event.Effect = securityaudit.EffectAllow
	event.Reasons = []string{"kind." + string(redemption.Kind)}
	event.Arrangement = securityaudit.ArrangementC
	return event
}

func (b *Broker) baseEvent(kind securityaudit.EventType, action string, req RedeemRequest, facts requestFacts) securityaudit.SecurityEvent {
	r := facts.request
	event := securityaudit.SecurityEvent{
		Type: kind, Time: b.now().UTC(), TenantProjectID: req.Run.TenantProjectID, Action: action,
		PrincipalRef: r.Subject.PrincipalRef, CallingApplicationID: bounded(r.Subject.CallingApplicationID),
		AgentBindingID: r.Agent.BindingID, WorkloadID: bounded(req.Run.WorkloadID),
		ExecutionID: req.Run.ExecutionID, InvocationID: req.Invocation.InvocationID,
		ResourceType: "credential_reference", ResourceID: req.Invocation.CredentialReferenceID,
		PolicyRevisionID: r.Context.PolicyRevisionID, ActorFidelity: securityaudit.FidelityNone,
	}
	if r.Subject.Attributed {
		event.ActorFidelity = securityaudit.FidelityAttributed
	}
	for _, link := range facts.actorChain {
		if len(event.ActorChain) == 32 {
			break
		}
		event.ActorChain = append(event.ActorChain, bounded(link))
	}
	switch r.Authority.Kind {
	case authzcompose.AuthorityMandate:
		event.MandateRef, event.RevocationEpoch = r.Authority.ID, r.Authority.Epoch
	case authzcompose.AuthorityDelegation:
		event.DelegationRef, event.RevocationEpoch = r.Authority.ID, r.Authority.Epoch
	}
	return event
}

// bounded keeps an audit string inside the event's 256-byte bound.
func bounded(value string) string {
	if len(value) > 256 || strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 }) {
		return ""
	}
	return value
}

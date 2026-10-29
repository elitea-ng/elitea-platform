package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// rowFunc is QueryRow for either transaction type an admission path holds
// (pgx.Tx or the shared store's sqlExecutor), whose QueryRow results differ
// only in their interface name.
type rowFunc func(ctx context.Context, sql string, args ...any) sqlRow

func pgxRows(tx pgx.Tx) rowFunc {
	return func(ctx context.Context, sql string, args ...any) sqlRow { return tx.QueryRow(ctx, sql, args...) }
}

func sharedRows(tx sqlExecutor) rowFunc {
	return tx.QueryRow
}

func poolRows(pool *pgxpool.Pool) rowFunc {
	return func(ctx context.Context, sql string, args ...any) sqlRow { return pool.QueryRow(ctx, sql, args...) }
}

// securityContextFacts is what an admission knows about the run beyond its
// pin. applicationVersionID is set for an agent run only.
type securityContextFacts struct {
	tenantProjectID      int64
	actorID              string
	applicationID        int64
	applicationVersionID int64
	// bindingVersionID is the version whose agent binding the run binds
	// when it is not applicationVersionID: the release's own version row
	// for a release run (ADR-0040 decision 2). Zero is applicationVersionID.
	bindingVersionID int64
	admittedAt       time.Time
	// governed is the agent run's governed credential freeze (#1119); nil
	// when admission resolved none.
	governed *agentexecutionapp.GovernedCredentialFreeze
}

// ErrGovernedBindingChanged refuses an admission whose transaction binds a
// different agent binding than the one its governed credentials were
// resolved for (#1119): a binding was approved, suspended or replaced in
// between. A retry resolves again.
var ErrGovernedBindingChanged = errors.New("the agent binding changed while governed credentials were resolved")

// bindingVersion is the version whose agent binding the run binds.
func (facts securityContextFacts) bindingVersion() int64 {
	if facts.bindingVersionID > 0 {
		return facts.bindingVersionID
	}
	return facts.applicationVersionID
}

// recordFacts reads the facts every admission record carries.
func recordFacts(record executiondomain.Admission, admittedAt time.Time, applicationVersionID int64) securityContextFacts {
	tenant, _ := strconv.ParseInt(record.Job.ResourceProjectID, 10, 64)
	return securityContextFacts{
		tenantProjectID:      tenant,
		actorID:              record.Job.ActorID,
		applicationVersionID: applicationVersionID,
		admittedAt:           admittedAt,
	}
}

// admitExecutionProfile pins the execution (pinExecutionAdmission) and, when
// the pin it ends up with is governed, stores the execution's security
// context and returns its reference for WorkerCommandV1.security_context_ref
// (#1115). A legacy admission returns the zero reference and, as before,
// sends no statement when the deployment never ran governed.
func admitExecutionProfile(ctx context.Context, tx pinExecutor, rows rowFunc, admission profile.Admission, pin admissionPin, facts securityContextFacts) (securitycontext.Reference, error) {
	if err := pinExecutionAdmission(ctx, tx, admission, pin); err != nil {
		return securitycontext.Reference{}, err
	}
	if !admission.Governed() && !admission.ContinuationGuard() {
		return securitycontext.Reference{}, nil
	}
	return recordSecurityContext(ctx, tx, rows, pin.executionID, facts)
}

// recordSecurityContext builds the context from the execution's pin and the
// run's active agent binding, inside the admission transaction, and stores
// it. An execution whose pin is legacy (or absent) gets none. A re-admission
// of the same execution returns the context stored first.
func recordSecurityContext(ctx context.Context, tx pinExecutor, rows rowFunc, executionID string, facts securityContextFacts) (securitycontext.Reference, error) {
	var revisionID *string
	var digest []byte
	err := rows(ctx, `
SELECT policy_revision_id, policy_digest
FROM elitea_runtime.execution_admission_profiles
WHERE execution_id = $1 AND profile = 'elitea.credential-profile.governed.v1'`, executionID).Scan(&revisionID, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return securitycontext.Reference{}, nil
	}
	if err != nil {
		return securitycontext.Reference{}, fmt.Errorf("read governed admission pin: %w", err)
	}
	if revisionID == nil || len(digest) != len(runtimedomain.Digest{}) {
		return securitycontext.Reference{}, errors.New("governed admission pin has no revision")
	}
	if stored, ok, err := storedSecurityContext(ctx, rows, executionID); err != nil || ok {
		return stored, err
	}
	built := securitycontext.Context{
		PolicyRevisionID:    *revisionID,
		PolicyDigest:        runtimedomain.Digest(digestArray(digest)),
		TenantProjectID:     facts.tenantProjectID,
		SubjectPrincipalRef: facts.actorID,
		Caller:              callerFromContext(ctx),
		IssuedAt:            facts.admittedAt,
	}
	if authority, ok := securitycontext.RunAuthorityFrom(ctx); ok {
		// An unattended governed run (#1122): its authority is the attached
		// mandate, never one inferred from the run's user. The context value
		// reaches here because admission runs in the entry point's call.
		if err := bindMandateAuthority(ctx, rows, &built, facts, authority); err != nil {
			return securitycontext.Reference{}, err
		}
	} else if err := bindInferredAuthority(ctx, rows, &built, facts); err != nil {
		return securitycontext.Reference{}, err
	}
	if facts.applicationID > 0 && facts.applicationVersionID > 0 {
		built.ApplicationID, built.ApplicationVersionID = facts.applicationID, facts.applicationVersionID
	}
	if governed := facts.governed; governed != nil {
		if governed.AgentBindingID != built.AgentBindingID || governed.ApplicationVersionID != facts.bindingVersion() {
			return securitycontext.Reference{}, ErrGovernedBindingChanged
		}
		for _, ref := range governed.References {
			built.CredentialReferences = append(built.CredentialReferences, securitycontext.CredentialReference{
				ToolkitID: ref.ToolkitID, CredentialReferenceID: ref.CredentialReferenceID,
				TargetBindingID: ref.TargetBindingID, Generation: ref.Generation,
				ToolkitDigest: runtimedomain.Digest(ref.ToolkitDigest),
			})
		}
	}
	encoded, reference, err := built.Marshal(executionID)
	if err != nil {
		return securitycontext.Reference{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_security_contexts (
    execution_id, content_id, schema_revision, digest, context_bytes
) VALUES ($1, $2, $3, $4, $5)`,
		executionID, reference.ContentID, reference.SchemaRevision, reference.Digest[:], encoded,
	); err != nil {
		return securitycontext.Reference{}, fmt.Errorf("store execution security context: %w", err)
	}
	return reference, nil
}

// bindInferredAuthority is the authority of a run that names none: the
// run's user, the version's usable binding and the delegation or mandate
// bindRunAuthority finds for them.
func bindInferredAuthority(ctx context.Context, rows rowFunc, built *securitycontext.Context, facts securityContextFacts) error {
	if facts.actorID != "" {
		built.ActorChain = []securitycontext.ActorLink{{Kind: "user", Ref: facts.actorID}}
	}
	if facts.applicationVersionID > 0 {
		if err := bindActiveAgentIdentity(ctx, rows, built, facts); err != nil {
			return err
		}
	}
	if built.AgentBindingID != "" {
		return bindRunAuthority(ctx, rows, built, facts)
	}
	return nil
}

// ErrRunAuthorityNotUsable refuses the admission of an unattended governed
// run whose attached mandate changed after its fire-time check: revoked,
// suspended, expired, at another epoch, or bound to an agent that is not
// usable or is not the run's version (#1122). The admission transaction
// rolls back, so the run writes no row.
var ErrRunAuthorityNotUsable = errors.New("the run's mandate is not usable")

// bindMandateAuthority names the attached mandate, its binding and its
// delegation in the context, read in the admission transaction. The subject
// is the mandate's: empty for an autonomous mandate, whatever user the run
// is attributed to.
func bindMandateAuthority(ctx context.Context, rows rowFunc, built *securitycontext.Context, facts securityContextFacts, authority securitycontext.RunAuthority) error {
	var (
		subject                    *int64
		mandateEpoch, bindingEpoch int64
		bindingID, delegationID    string
		versionID                  int64
		digest                     []byte
		delegationEpoch            *int64
	)
	err := rows(ctx, `
SELECT m.subject_user_id, m.revocation_epoch, b.id::text, b.revocation_epoch, b.definition_digest,
       b.application_version_id, coalesce(m.delegation_id::text, ''), d.revocation_epoch
FROM elitea_identity.mandates AS m
JOIN elitea_identity.agent_identity_bindings AS b
  ON b.id = m.agent_binding_id AND b.tenant_project_id = m.tenant_project_id AND b.state = 'active'
JOIN elitea_identity.external_agent_identities AS e
  ON e.id = b.external_agent_identity_id AND e.tenant_project_id = b.tenant_project_id AND e.state = 'active'
LEFT JOIN elitea_identity.delegations AS d
  ON d.id = m.delegation_id AND d.tenant_project_id = m.tenant_project_id
 AND d.state = 'active' AND d.expires_at > $3
WHERE m.id::text = $1 AND m.tenant_project_id = $2 AND m.state = 'active'
  AND m.not_before <= $3 AND m.expires_at > $3
  AND m.kind IN ('autonomous', 'delegated_background')`,
		authority.MandateID, facts.tenantProjectID, facts.admittedAt,
	).Scan(&subject, &mandateEpoch, &bindingID, &bindingEpoch, &digest, &versionID, &delegationID, &delegationEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunAuthorityNotUsable
	}
	if err != nil {
		return fmt.Errorf("read the run's mandate: %w", err)
	}
	subjectID := int64(0)
	if subject != nil {
		subjectID = *subject
	}
	switch {
	case mandateEpoch != authority.MandateEpoch,
		subjectID != authority.SubjectUserID,
		len(digest) != len(runtimedomain.Digest{}),
		facts.applicationVersionID > 0 && versionID != facts.bindingVersion(),
		delegationID != "" && delegationEpoch == nil,
		// The mandate's binding pins another definition than the one the
		// run executes (#1462).
		definitionMismatch(facts, runtimedomain.Digest(digestArray(digest))):
		return ErrRunAuthorityNotUsable
	}
	built.SubjectPrincipalRef, built.ActorChain = "", nil
	if subjectID > 0 {
		built.SubjectPrincipalRef = strconv.FormatInt(subjectID, 10)
		built.ActorChain = []securitycontext.ActorLink{{Kind: "user", Ref: built.SubjectPrincipalRef}}
	}
	if authority.Caller != (securitycontext.Caller{}) {
		built.Caller = authority.Caller
	}
	built.AgentBindingID, built.AgentBindingEpoch = bindingID, bindingEpoch
	built.DefinitionDigest = runtimedomain.Digest(digestArray(digest))
	built.MandateID, built.MandateEpoch = authority.MandateID, mandateEpoch
	if delegationID != "" {
		built.DelegationID, built.DelegationEpoch = delegationID, *delegationEpoch
	}
	return nil
}

// bindActiveAgentIdentity names the agent version's usable binding in the
// context: active, with an active external agent identity
// (agentidentity.UsableBinding). Exactly one usable binding binds the run. None, or more than one
// (two definition digests of one version), leaves the binding empty: the
// context records that no binding was proven, and the decision (#1116)
// denies every governed credential use of the run.
//
// With a governed freeze that checked the definition (#1462), a binding
// whose digest is not the computed digest of the version the run executes
// is not bound either: the run has no agent identity, and every governed
// credential use is denied. Freeze recorded the mismatch in the security
// audit; the comparison is repeated here because the binding is read again
// in this transaction.
func bindActiveAgentIdentity(ctx context.Context, rows rowFunc, built *securitycontext.Context, facts securityContextFacts) error {
	id, epoch, digest, found, err := usableBindingForVersion(ctx, rows, facts.tenantProjectID, facts.bindingVersion())
	if err != nil || !found {
		return err
	}
	if definitionMismatch(facts, digest) {
		return nil
	}
	built.AgentBindingID, built.AgentBindingEpoch = id, epoch
	built.DefinitionDigest = digest
	return nil
}

// definitionMismatch reports whether the admission's governed freeze
// computed the run's definition digest (#1462) and the binding pins
// another. A freeze that did not check (no binding when it ran) or no freeze
// at all compares nothing, as before.
func definitionMismatch(facts securityContextFacts, bound runtimedomain.Digest) bool {
	governed := facts.governed
	return governed != nil && governed.DefinitionChecked && [32]byte(bound) != governed.DefinitionDigest
}

// UsableBindingForVersion answers the usable binding the admission
// transaction binds for an application version, with its definition digest
// (agentexecution.GovernedAgentBindings, #1119, #1462).
func (s *AgentIdentityBindings) UsableBindingForVersion(ctx context.Context, tenantProjectID, applicationVersionID int64) (string, [32]byte, bool, error) {
	if s == nil || s.pool == nil {
		return "", [32]byte{}, false, errors.New("agent identity bindings: no database pool")
	}
	id, _, digest, found, err := usableBindingForVersion(ctx, poolRows(s.pool), tenantProjectID, applicationVersionID)
	return id, [32]byte(digest), found, err
}

// usableBindingForVersion is bindActiveAgentIdentity's rule: the one usable
// binding of the version. None, more than one, or a malformed digest is
// found=false.
func usableBindingForVersion(ctx context.Context, rows rowFunc, tenantProjectID, applicationVersionID int64) (string, int64, runtimedomain.Digest, bool, error) {
	var (
		id      string
		epoch   int64
		digest  []byte
		matches int
	)
	err := rows(ctx, `
SELECT b.id::text, b.revocation_epoch, b.definition_digest, count(*) OVER ()
FROM elitea_identity.agent_identity_bindings AS b
JOIN elitea_identity.external_agent_identities AS e
  ON e.id = b.external_agent_identity_id AND e.tenant_project_id = b.tenant_project_id
WHERE b.tenant_project_id = $1 AND b.application_version_id = $2 AND b.state = 'active'
  AND e.state = 'active'
ORDER BY b.id
LIMIT 1`, tenantProjectID, applicationVersionID).Scan(&id, &epoch, &digest, &matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, runtimedomain.Digest{}, false, nil
	}
	if err != nil {
		return "", 0, runtimedomain.Digest{}, false, fmt.Errorf("read active agent identity binding: %w", err)
	}
	if matches != 1 || len(digest) != len(runtimedomain.Digest{}) {
		return "", 0, runtimedomain.Digest{}, false, nil
	}
	return id, epoch, runtimedomain.Digest(digestArray(digest)), true, nil
}

func storedSecurityContext(ctx context.Context, rows rowFunc, executionID string) (securitycontext.Reference, bool, error) {
	var reference securitycontext.Reference
	var digest []byte
	err := rows(ctx, `
SELECT content_id, schema_revision, digest
FROM elitea_runtime.execution_security_contexts
WHERE execution_id = $1`, executionID).Scan(&reference.ContentID, &reference.SchemaRevision, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return securitycontext.Reference{}, false, nil
	}
	if err != nil {
		return securitycontext.Reference{}, false, fmt.Errorf("read execution security context: %w", err)
	}
	if len(digest) != len(reference.Digest) {
		return securitycontext.Reference{}, false, errors.New("stored execution security context digest is malformed")
	}
	copy(reference.Digest[:], digest)
	return reference, true, nil
}

// loadSecurityContextReference is the dispatch-time read of field 65 for the
// command types prepared after admission. It sends no statement unless this
// deployment can hold governed pins (the flag is on, or the continuation
// guard saw governed pins at boot), so a flag-off dispatch is unchanged.
func loadSecurityContextReference(ctx context.Context, rows rowFunc, admission profile.Admission, executionID string) (securitycontext.Reference, error) {
	if !admission.Governed() && !admission.ContinuationGuard() {
		return securitycontext.Reference{}, nil
	}
	reference, _, err := storedSecurityContext(ctx, rows, executionID)
	return reference, err
}

// callerFromContext reads the authentication provenance the request
// middleware recorded. A run no authenticated request started (a schedule,
// a server-side continuation) has none.
func callerFromContext(ctx context.Context) securitycontext.Caller {
	var caller securitycontext.Caller
	source, ok := auth.AuthenticationSourceFromContext(ctx)
	if !ok {
		return caller
	}
	switch source {
	case auth.AuthenticationSourceSession:
		caller.AuthenticationSource = "session"
	case auth.AuthenticationSourceToken:
		caller.AuthenticationSource = "token"
	case auth.AuthenticationSourceAPIKey:
		caller.AuthenticationSource = "api_key"
	case auth.AuthenticationSourceDevelopment:
		caller.AuthenticationSource = "development"
	}
	if user, ok := auth.UserFromContext(ctx); ok && user.TokenID != "" {
		caller.CallingApplicationID = "token:" + user.TokenID
	}
	// A verified inbound MCP OAuth token names its client (#1123).
	if application, ok := auth.CallingApplicationFromContext(ctx); ok {
		caller.CallingApplicationID = application
	}
	return caller
}

// bindRunAuthority names the one authority source of the run in the context
// (ADR-0039 decision 3), read in the admission transaction. In order:
//
//  1. A run with a user: that user's newest active, unexpired delegation for
//     the bound agent. A delegation names the IdP subject (tid + oid), not
//     the Elitea user, so it is the user's through the sign-in assertion of
//     the browser session that created it.
//  2. Else, for a run with a user: exactly one active delegated_background
//     mandate of the binding naming that user; its delegation, if any, too.
//  3. A run with no user: exactly one active autonomous mandate of the
//     binding.
//
// Nothing, or more than one mandate, records no authority: the decision
// (check 6) then denies every governed credential use of the run. An
// autonomous mandate never serves a run that has a user.
func bindRunAuthority(ctx context.Context, rows rowFunc, built *securitycontext.Context, facts securityContextFacts) error {
	userID, _ := strconv.ParseInt(facts.actorID, 10, 64)
	if userID > 0 {
		var id string
		var epoch int64
		err := rows(ctx, `
SELECT d.id::text, d.revocation_epoch
FROM elitea_identity.delegations AS d
WHERE d.tenant_project_id = $1 AND d.agent_binding_id = $2::uuid
  AND d.state = 'active' AND d.expires_at > $4
  AND EXISTS (
      SELECT 1 FROM elitea_identity.signin_assertions AS a
      WHERE a.browser_session_digest = d.browser_session_digest AND a.user_id = $3
        AND a.tenant_id = d.tenant_id AND a.subject_object_id = d.subject_object_id)
ORDER BY d.created_at DESC, d.id
LIMIT 1`, facts.tenantProjectID, built.AgentBindingID, userID, facts.admittedAt).Scan(&id, &epoch)
		switch {
		case err == nil:
			built.DelegationID, built.DelegationEpoch = id, epoch
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read the run's delegation: %w", err)
		}
	}
	var (
		mandateID, delegationID string
		mandateEpoch            int64
		delegationEpoch         *int64
		matches                 int
	)
	err := rows(ctx, `
SELECT m.id::text, m.revocation_epoch, coalesce(m.delegation_id::text, ''), d.revocation_epoch, count(*) OVER ()
FROM elitea_identity.mandates AS m
LEFT JOIN elitea_identity.delegations AS d
  ON d.id = m.delegation_id AND d.tenant_project_id = m.tenant_project_id
WHERE m.tenant_project_id = $1 AND m.agent_binding_id = $2::uuid AND m.state = 'active'
  AND m.not_before <= $4 AND m.expires_at > $4
  AND CASE WHEN $3::bigint > 0
           THEN m.kind = 'delegated_background' AND m.subject_user_id = $3
           ELSE m.kind = 'autonomous' END
ORDER BY m.id
LIMIT 1`, facts.tenantProjectID, built.AgentBindingID, userID, facts.admittedAt).
		Scan(&mandateID, &mandateEpoch, &delegationID, &delegationEpoch, &matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the run's mandate: %w", err)
	}
	if matches != 1 {
		return nil
	}
	built.MandateID, built.MandateEpoch = mandateID, mandateEpoch
	if delegationID != "" && delegationEpoch != nil {
		built.DelegationID, built.DelegationEpoch = delegationID, *delegationEpoch
	}
	return nil
}

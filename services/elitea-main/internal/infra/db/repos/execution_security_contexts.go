package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	applicationVersionID int64
	admittedAt           time.Time
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
	if facts.actorID != "" {
		built.ActorChain = []securitycontext.ActorLink{{Kind: "user", Ref: facts.actorID}}
	}
	if facts.applicationVersionID > 0 {
		if err := bindActiveAgentIdentity(ctx, rows, &built, facts); err != nil {
			return securitycontext.Reference{}, err
		}
	}
	if built.AgentBindingID != "" {
		if err := bindRunAuthority(ctx, rows, &built, facts); err != nil {
			return securitycontext.Reference{}, err
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

// bindActiveAgentIdentity names the agent version's usable binding in the
// context: active, with an active external agent identity
// (agentidentity.UsableBinding). Exactly one usable binding binds the run. None, or more than one
// (two definition digests of one version), leaves the binding empty: the
// context records that no binding was proven, and the decision (#1116)
// denies every governed credential use of the run.
func bindActiveAgentIdentity(ctx context.Context, rows rowFunc, built *securitycontext.Context, facts securityContextFacts) error {
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
LIMIT 1`, facts.tenantProjectID, facts.applicationVersionID).Scan(&id, &epoch, &digest, &matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active agent identity binding: %w", err)
	}
	if matches != 1 || len(digest) != len(runtimedomain.Digest{}) {
		return nil
	}
	built.AgentBindingID, built.AgentBindingEpoch = id, epoch
	built.DefinitionDigest = runtimedomain.Digest(digestArray(digest))
	return nil
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

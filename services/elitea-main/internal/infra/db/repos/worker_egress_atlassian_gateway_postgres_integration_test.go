package repos

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

const (
	gatewayExternal  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1464"
	gatewayBinding   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1465"
	gatewayJira      = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1466"
	gatewayStatic    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1467"
	gatewayOAuth     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1468"
	gatewayStaticRef = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1469"
	// A target with a frozen static reference and a retired OAuth one.
	gatewayShared        = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1470"
	gatewaySharedStatic  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1471"
	gatewaySharedRetired = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1472"
	// A target whose OAuth reference has only an expired grant.
	gatewayExpired    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1473"
	gatewayExpiredRef = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1474"
)

// #1464: a governed run that froze an oauth_delegated Jira reference gets
// the Atlassian gateway in its allowlist (the worker calls
// api.atlassian.com/ex/jira/<cloud id> with the token); a run that froze
// only a static Jira PAT does not. Only the frozen reference counts, and
// only while it is active with an active, unexpired use grant.
func TestPostgresGovernedEgressTargetsAddTheAtlassianGatewayForOAuth(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-egress-gateway")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-gateway', 'active', '100', '200')`, gatewayExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		gatewayBinding, bytes.Repeat([]byte{9}, 32), gatewayExternal, revision.ID)
	exec(`
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, 7, 'jira', 'https://acme.atlassian.net', ''),
       ($2::uuid, 7, 'jira', 'https://pat.atlassian.net', ''),
       ($3::uuid, 7, 'jira', 'https://shared.atlassian.net', ''),
       ($4::uuid, 7, 'jira', 'https://expired.atlassian.net', '')`,
		gatewayJira, gatewayStatic, gatewayShared, gatewayExpired)
	exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'oauth_delegated', 'pgvault', 'oauth_grant', 'jira', $2::uuid, 0, 'active'),
       ($3::uuid, 7, 'static_secret', 'pgvault', 'p7_jira', 'jira', $4::uuid, 1, 'active'),
       ($5::uuid, 7, 'static_secret', 'pgvault', 'p7_shared', 'jira', $6::uuid, 1, 'active'),
       ($7::uuid, 7, 'oauth_delegated', 'pgvault', 'oauth_grant', 'jira', $6::uuid, 0, 'retired'),
       ($8::uuid, 7, 'oauth_delegated', 'pgvault', 'oauth_grant', 'jira', $9::uuid, 0, 'active')`,
		gatewayOAuth, gatewayJira, gatewayStaticRef, gatewayStatic,
		gatewaySharedStatic, gatewayShared, gatewaySharedRetired, gatewayExpiredRef, gatewayExpired)
	// Active use grants of the agent binding: one per frozen reference, one
	// for the retired OAuth reference (toolkit 23 also holds it), and an
	// expired one for toolkit 25.
	grant := func(toolkitID int64, reference, target, expires string) {
		t.Helper()
		exec(`
INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, epoch, expires_at, proposed_by, approved_by, last_transition_by)
VALUES (gen_random_uuid(), 7, $1::uuid, $2, $3::uuid, $4::uuid, ARRAY['use'], ARRAY['jira.issue.read'],
        'active', 1, NULLIF($5, '')::timestamptz, '100', '200', '200')`,
			gatewayBinding, toolkitID, reference, target, expires)
	}
	grant(21, gatewayOAuth, gatewayJira, "")
	grant(22, gatewayStaticRef, gatewayStatic, "")
	grant(23, gatewaySharedStatic, gatewayShared, "")
	grant(23, gatewaySharedRetired, gatewayShared, "")
	grant(25, gatewayExpiredRef, gatewayExpired, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))

	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	targetsOf := func(executionID string, frozen ...*runtimev1.GovernedCredentialReferenceV1) []string {
		t.Helper()
		contextBytes, err := proto.Marshal(&runtimev1.ExecutionSecurityContextV1{
			SchemaRevision: securitycontext.SchemaRevisionV1, TenantProjectId: 7, AgentBindingId: gatewayBinding,
			CredentialReferences: frozen,
		})
		if err != nil {
			t.Fatal(err)
		}
		digest := runtimedomain.SHA256(contextBytes)
		exec(`
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ($1, $2, 'elitea.runtime.security-context.v1', $3, $4)`,
			executionID, securitycontext.ContentIDPrefix+executionID, digest[:], contextBytes)
		targets, err := sources.GovernedEgressTargets(ctx, &runtimev1.ExecutionSecurityContextReferenceV1{
			ContentEntryId: securitycontext.ContentIDPrefix + executionID,
			Digest:         &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]},
			SchemaRevision: securitycontext.SchemaRevisionV1,
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(targets)
		return targets
	}

	gateway := provideroauth.AtlassianAPIURL
	oauth := targetsOf("egress-gateway-oauth",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 21, CredentialReferenceId: gatewayOAuth, TargetBindingId: gatewayJira})
	if want := []string{"https://acme.atlassian.net", gateway}; !slices.Equal(oauth, want) {
		t.Fatalf("oauth targets %v, want %v", oauth, want)
	}
	static := targetsOf("egress-gateway-static",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 22, CredentialReferenceId: gatewayStaticRef, TargetBindingId: gatewayStatic})
	if want := []string{"https://pat.atlassian.net"}; !slices.Equal(static, want) {
		t.Fatalf("static targets %v, want %v", static, want)
	}
	// Review of #1556: the run froze the static reference. A retired OAuth
	// reference for the same target, even with a live grant, does not
	// open the gateway.
	shared := targetsOf("egress-gateway-shared",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 23, CredentialReferenceId: gatewaySharedStatic, TargetBindingId: gatewayShared})
	if want := []string{"https://shared.atlassian.net"}; !slices.Equal(shared, want) {
		t.Fatalf("shared targets %v, want %v", shared, want)
	}
	// The same toolkit frozen without a reference resolves to its usable
	// grants: the retired OAuth reference still does not count.
	unresolved := targetsOf("egress-gateway-unresolved",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 23, TargetBindingId: gatewayShared})
	if want := []string{"https://shared.atlassian.net"}; !slices.Equal(unresolved, want) {
		t.Fatalf("unresolved targets %v, want %v", unresolved, want)
	}
	// An active OAuth reference whose only grant has expired does not open
	// the gateway.
	expired := targetsOf("egress-gateway-expired",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 25, CredentialReferenceId: gatewayExpiredRef, TargetBindingId: gatewayExpired})
	if slices.Contains(expired, gateway) {
		t.Fatalf("an expired grant opened the gateway: %v", expired)
	}
	// A frozen OAuth reference of another toolkit than its grant's does not
	// open it either.
	other := targetsOf("egress-gateway-other-toolkit",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 26, CredentialReferenceId: gatewayOAuth, TargetBindingId: gatewayJira})
	if slices.Contains(other, gateway) {
		t.Fatalf("a reference without a grant for its toolkit opened the gateway: %v", other)
	}
}

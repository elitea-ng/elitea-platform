package repos

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workeregress"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	gatewayExternal  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1464"
	gatewayBinding   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1465"
	gatewayJira      = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1466"
	gatewayStatic    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1467"
	gatewayOAuth     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1468"
	gatewayStaticRef = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1469"
)

// #1464: a governed run that froze an oauth_delegated Jira reference gets
// the Atlassian gateway in its allowlist (the worker calls
// api.atlassian.com/ex/jira/<cloud id> with the token); a run that froze
// only a static Jira PAT does not.
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
       ($2::uuid, 7, 'jira', 'https://pat.atlassian.net', '')`, gatewayJira, gatewayStatic)
	exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'oauth_delegated', 'pgvault', 'oauth_grant', 'jira', $2::uuid, 0, 'active'),
       ($3::uuid, 7, 'static_secret', 'pgvault', 'p7_jira', 'jira', $4::uuid, 1, 'active')`,
		gatewayOAuth, gatewayJira, gatewayStaticRef, gatewayStatic)

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

	oauth := targetsOf("egress-gateway-oauth",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 21, CredentialReferenceId: gatewayOAuth, TargetBindingId: gatewayJira})
	if want := []string{"https://acme.atlassian.net", workeregress.GovernedOAuthGatewayURL}; !slices.Equal(oauth, want) {
		t.Fatalf("oauth targets %v, want %v", oauth, want)
	}
	static := targetsOf("egress-gateway-static",
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 22, CredentialReferenceId: gatewayStaticRef, TargetBindingId: gatewayStatic})
	if want := []string{"https://pat.atlassian.net"}; !slices.Equal(static, want) {
		t.Fatalf("static targets %v, want %v", static, want)
	}
}

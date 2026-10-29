package repos

import (
	"bytes"
	"context"
	"errors"
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
	egressGovExternal   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1490"
	egressGovBinding    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1491"
	egressGovJira       = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1492"
	egressGovSharePoint = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1493"
	egressGovReference  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1494"
	egressGovGrant      = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1495"
	egressGovForeign    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1496"
)

// The governed run's credential targets come from its frozen security
// context, never from settings: a frozen target binding's identity, a
// digest-only toolkit's usable grants' targets with their audience, and
// nothing of another tenant. A context that does not match its reference is
// a refusal.
func TestPostgresGovernedEgressTargetsReadTheFrozenContext(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-egress-targets")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-egress', 'active', '100', '200')`, egressGovExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		egressGovBinding, bytes.Repeat([]byte{9}, 32), egressGovExternal, revision.ID)
	exec(`
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, 7, 'jira', 'https://jira.corp.example', ''),
       ($2::uuid, 7, 'sharepoint', 'https://contoso.sharepoint.com/sites/eng', 'https://graph.microsoft.com'),
       ($3::uuid, 8, 'jira', 'https://foreign.example', '')`,
		egressGovJira, egressGovSharePoint, egressGovForeign)
	exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', 'p7/sharepoint', 'sharepoint', $2::uuid, 1, 'active')`,
		egressGovReference, egressGovSharePoint)
	exec(`INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, 23, 7, $2)`, egressGovBinding, bytes.Repeat([]byte{5}, 32))
	exec(`
INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, epoch, proposed_by, approved_by, last_transition_by)
VALUES ($1::uuid, 7, $2::uuid, 23, $3::uuid, $4::uuid, ARRAY['use'], ARRAY['sharepoint.read'], 'active', 1, '100', '200', '200')`,
		egressGovGrant, egressGovBinding, egressGovReference, egressGovSharePoint)

	stored := &runtimev1.ExecutionSecurityContextV1{
		SchemaRevision: securitycontext.SchemaRevisionV1, TenantProjectId: 7, AgentBindingId: egressGovBinding,
		CredentialReferences: []*runtimev1.GovernedCredentialReferenceV1{
			// Frozen with its target.
			{ToolkitId: 21, CredentialReferenceId: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1497", TargetBindingId: egressGovJira},
			// Digest-only: the grant is resolved per operation.
			{ToolkitId: 23},
			// Another tenant's target is never read.
			{ToolkitId: 22, CredentialReferenceId: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1498", TargetBindingId: egressGovForeign},
		},
	}
	contextBytes, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	digest := runtimedomain.SHA256(contextBytes)
	exec(`
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ('egress-gov-1', 'elitea.security-context/egress-gov-1', 'elitea.runtime.security-context.v1', $1, $2)`,
		digest[:], contextBytes)

	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	reference := func(executionID string, digest runtimedomain.Digest) *runtimev1.ExecutionSecurityContextReferenceV1 {
		return &runtimev1.ExecutionSecurityContextReferenceV1{
			ContentEntryId: securitycontext.ContentIDPrefix + executionID,
			Digest:         &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]},
			SchemaRevision: securitycontext.SchemaRevisionV1,
		}
	}
	targets, err := sources.GovernedEgressTargets(ctx, reference("egress-gov-1", digest))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(targets)
	want := []string{"https://contoso.sharepoint.com/sites/eng", "https://graph.microsoft.com", "https://jira.corp.example"}
	if !slices.Equal(targets, want) {
		t.Fatalf("targets %v, want %v", targets, want)
	}

	// A revoked grant no longer reaches the digest-only toolkit's target.
	exec(`UPDATE elitea_identity.credential_use_grants SET state = 'revoked', epoch = epoch + 1 WHERE id = $1::uuid`, egressGovGrant)
	targets, err = sources.GovernedEgressTargets(ctx, reference("egress-gov-1", digest))
	if err != nil || !slices.Equal(targets, []string{"https://jira.corp.example"}) {
		t.Fatalf("after revocation %v err=%v", targets, err)
	}

	wrong := digest
	wrong[0] ^= 0xff
	for name, ref := range map[string]*runtimev1.ExecutionSecurityContextReferenceV1{
		"another digest":   reference("egress-gov-1", wrong),
		"no such context":  reference("egress-gov-missing", digest),
		"not a content id": {ContentEntryId: "egress-gov-1", Digest: &runtimev1.DigestV1{Value: digest[:]}},
	} {
		if _, err := sources.GovernedEgressTargets(ctx, ref); !errors.Is(err, workeregress.ErrSecurityContextInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

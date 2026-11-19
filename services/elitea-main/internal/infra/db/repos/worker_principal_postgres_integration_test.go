package repos

// The run principal's bound (ADR-0044 Amendment 1, #2157) against a REAL
// migrated database: the closure of a root through attachments (cycle
// included), the folder access that hides a toolkit from one user, the
// credential references an active use grant lets the closure use, and the
// principal a command names. It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	principalRefLive    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1501"
	principalRefExpired = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1502"
	principalRefSuspend = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1503"
	principalRefOutside = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1504"
)

func TestPostgresPrincipalSourcesReadRealAccess(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}

	// Project 7: agent 5 (version 9) holds toolkit 7 and attaches agent 6
	// (version 10), which holds toolkits 8 and 9 and attaches agent 5 back (a
	// cycle). Agent 4 (version 11) holds toolkit 99 and is not reachable.
	exec(`
CREATE SCHEMA p_7;
CREATE TABLE p_7.applications (id SERIAL PRIMARY KEY, name VARCHAR);
CREATE TABLE p_7.application_versions (
    id SERIAL PRIMARY KEY, application_id INTEGER NOT NULL, agent_type VARCHAR, instructions TEXT);
CREATE TABLE p_7.elitea_tools (
    id SERIAL PRIMARY KEY, type VARCHAR NOT NULL, name VARCHAR(128), settings JSONB NOT NULL DEFAULT '{}');
CREATE TABLE p_7.entity_tool_mapping (
    id SERIAL PRIMARY KEY, tool_id INTEGER NOT NULL, entity_id INTEGER NOT NULL,
    entity_version_id INTEGER NOT NULL, entity_type VARCHAR NOT NULL);
INSERT INTO p_7.applications (id, name) VALUES (4, 'unreachable'), (5, 'root'), (6, 'specialist');
INSERT INTO p_7.application_versions (id, application_id, agent_type) VALUES
    (9, 5, 'agent'), (10, 6, 'agent'), (11, 4, 'agent');
INSERT INTO p_7.elitea_tools (id, type, name, settings) VALUES
    (7, 'jira', 'Jira', '{}'), (8, 'github', 'Repo', '{}'), (9, 'confluence', 'Wiki', '{}'),
    (99, 'slack', 'Chat', '{}'),
    (55, 'application', 'specialist', '{"application_id":"6","application_version_id":"10"}'),
    (56, 'application', 'root', '{"application_id":"5","application_version_id":"9"}');
INSERT INTO p_7.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type) VALUES
    (7, 5, 9, 'agent'), (55, 5, 9, 'agent'),
    (8, 6, 10, 'agent'), (9, 6, 10, 'agent'), (56, 6, 10, 'agent'),
    (99, 4, 11, 'agent')`)

	// The closure: the input's own toolkit and every version reached through
	// attachments, the cycle walked once, an attached agent never a toolkit.
	reachable, err := sources.ReachableToolkits(ctx, 7, workerdelegation.ClosureSeeds{
		ToolkitIDs: []int64{7},
		Versions:   []applicationcomposition.Identity{{ApplicationID: 5, VersionID: 9}},
	})
	if err != nil || !slices.Equal(reachable, []int64{7, 8, 9}) {
		t.Fatalf("reachable = %v, err = %v; want [7 8 9]", reachable, err)
	}
	// An ad hoc conversation seeds toolkits only.
	adhoc, err := sources.ReachableToolkits(ctx, 7, workerdelegation.ClosureSeeds{ToolkitIDs: []int64{99, 7}})
	if err != nil || !slices.Equal(adhoc, []int64{7, 99}) {
		t.Fatalf("ad hoc = %v, err = %v", adhoc, err)
	}
	// A closure past the version limit is refused, never truncated.
	var many []applicationcomposition.Identity
	for i := 0; i <= workerdelegation.MaxClosureVersions; i++ {
		many = append(many, applicationcomposition.Identity{ApplicationID: int64(1000 + i), VersionID: int64(2000 + i)})
	}
	if _, err := sources.ReachableToolkits(ctx, 7, workerdelegation.ClosureSeeds{Versions: many}); !errors.Is(err, workerdelegation.ErrPrincipalBoundTooLarge) {
		t.Fatalf("oversize closure: %v", err)
	}

	// Without the social folder tables project RBAC is the authority: nothing
	// is hidden.
	if hidden, err := sources.HiddenToolkits(ctx, 7, 31, reachable); err != nil || len(hidden) != 0 {
		t.Fatalf("no folder access: hidden = %v, err = %v", hidden, err)
	}
	exec(`
CREATE TABLE p_7.entity_folders (id INTEGER PRIMARY KEY, entity_type VARCHAR(32) NOT NULL);
CREATE TABLE p_7.social_folder_items (id INTEGER PRIMARY KEY, folder_id INTEGER NOT NULL, entity VARCHAR(32) NOT NULL, entity_id INTEGER NOT NULL);
CREATE TABLE p_7.folder_access_overrides (id INTEGER PRIMARY KEY, folder_id INTEGER NOT NULL, user_id INTEGER NOT NULL, access_level VARCHAR(16) NOT NULL);
INSERT INTO p_7.entity_folders (id, entity_type) VALUES (3, 'toolkit'), (4, 'agent');
INSERT INTO p_7.social_folder_items (id, folder_id, entity, entity_id) VALUES
    (1, 3, 'toolkit', 9), (2, 3, 'mcp', 8), (3, 4, 'agent', 7);
INSERT INTO p_7.folder_access_overrides (id, folder_id, user_id, access_level) VALUES
    (1, 3, 31, 'no_access'), (2, 4, 31, 'no_access'), (3, 3, 32, 'view')`)
	// User 31 is hidden toolkits 8 (an MCP item) and 9; an agent folder with the
	// same id as toolkit 7 hides nothing. User 32 and an unnamed user see all.
	hidden, err := sources.HiddenToolkits(ctx, 7, 31, reachable)
	if err != nil || !slices.Equal(hidden, []int64{8, 9}) {
		t.Fatalf("user 31 hidden = %v, err = %v; want [8 9]", hidden, err)
	}
	for _, user := range []int64{32, 0} {
		if hidden, err := sources.HiddenToolkits(ctx, 7, user, reachable); err != nil || len(hidden) != 0 {
			t.Fatalf("user %d hidden = %v, err = %v", user, hidden, err)
		}
	}
	// An approver of a delegation grant is asked about the whole project, with no
	// id filter: the same two toolkits for user 31, none for the others.
	if all, err := sources.HiddenToolkitsForUser(ctx, 7, 31); err != nil || !slices.Equal(all, []int64{8, 9}) {
		t.Fatalf("user 31 hidden in the project = %v, err = %v; want [8 9]", all, err)
	}
	for _, user := range []int64{32, 0} {
		if all, err := sources.HiddenToolkitsForUser(ctx, 7, user); err != nil || len(all) != 0 {
			t.Fatalf("user %d hidden in the project = %v, err = %v", user, all, err)
		}
	}
	// Overrides without their dependencies are refused, not read as open.
	exec(`DROP TABLE p_7.social_folder_items`)
	if _, err := sources.HiddenToolkits(ctx, 7, 31, reachable); err == nil {
		t.Fatal("a partial folder access projection must be refused")
	}

	// The credential references an active, unexpired use grant of the project
	// lets a toolkit of the closure use.
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-principal")
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 7, 5, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-principal', 'active', '100', '200')`, egressGovExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 5, 9, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		egressGovBinding, bytes.Repeat([]byte{9}, 32), egressGovExternal, revision.ID)
	exec(`
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, 7, 'jira', 'https://jira.corp.example', '')`, egressGovJira)
	for _, reference := range []string{principalRefLive, principalRefExpired, principalRefSuspend, principalRefOutside} {
		exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', $2, 'jira', $3::uuid, 1, 'active')`,
			reference, "p7/"+reference, egressGovJira)
	}
	grant := func(id string, toolkit int, reference, state, expires string) {
		t.Helper()
		exec(`
INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, epoch, expires_at, proposed_by, approved_by, last_transition_by)
VALUES (('0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e16' || $1::text)::uuid, 7, $2::uuid, $3, $4::uuid, $5::uuid,
        ARRAY['use'], ARRAY['jira.read'], $6, 1, NULLIF($7, '')::timestamptz, '100', '200', '200')`,
			id, egressGovBinding, toolkit, reference, egressGovJira, state, expires)
	}
	grant("01", 7, principalRefLive, "active", "")
	grant("02", 8, principalRefExpired, "active", "2000-01-01T00:00:00Z")
	grant("03", 9, principalRefSuspend, "suspended", "")
	grant("04", 99, principalRefOutside, "active", "")
	references, err := sources.UsableCredentialReferences(ctx, 7, reachable)
	if err != nil || !slices.Equal(references, []string{principalRefLive}) {
		t.Fatalf("references = %v, err = %v; want only the live reference", references, err)
	}
	// The project-wide read (the approver's side) lists every active, unexpired
	// (toolkit, reference) pair, and none of another project's (#2215).
	wantUses := []ToolkitCredential{{ToolkitID: 7, Reference: principalRefLive}, {ToolkitID: 99, Reference: principalRefOutside}}
	if all, err := sources.UsableCredentialsInProject(ctx, 7); err != nil || !slices.Equal(all, wantUses) {
		t.Fatalf("project uses = %v, err = %v; want %v", all, err, wantUses)
	}
	if all, err := sources.UsableCredentialsInProject(ctx, 8); err != nil || len(all) != 0 {
		t.Fatalf("another project read the grants: %v, err = %v", all, err)
	}
	if references, err := sources.UsableCredentialReferences(ctx, 8, reachable); err != nil || len(references) != 0 {
		t.Fatalf("another project read the grants: %v, err = %v", references, err)
	}

	// The principal a command names: the actor of an ungoverned run, and the
	// stored, digest-checked context of a governed one (a mandate acts for
	// its subject user).
	principal, err := sources.RunPrincipal(ctx, &runtimev1.WorkerCommandV1{PrincipalRef: "31"})
	if err != nil || principal.UserID != 31 || principal.Governed || principal.MandateID != "" {
		t.Fatalf("ungoverned principal = %+v, err = %v", principal, err)
	}
	if principal, err := sources.RunPrincipal(ctx, &runtimev1.WorkerCommandV1{PrincipalRef: "system:scheduler"}); err != nil || principal.UserID != 0 {
		t.Fatalf("an actor that is no user = %+v, err = %v", principal, err)
	}
	stored := &runtimev1.ExecutionSecurityContextV1{
		SchemaRevision: securitycontext.SchemaRevisionV1, TenantProjectId: 7,
		SubjectPrincipalRef: "44", MandateId: "mandate-1",
		CredentialReferences: []*runtimev1.GovernedCredentialReferenceV1{
			{ToolkitId: 7, CredentialReferenceId: principalRefLive},
			{ToolkitId: 8},
		},
	}
	contextBytes, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	digest := runtimedomain.SHA256(contextBytes)
	exec(`
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ('principal-1', 'elitea.security-context/principal-1', 'elitea.runtime.security-context.v1', $1, $2)`,
		digest[:], contextBytes)
	reference := func(digest runtimedomain.Digest) *runtimev1.ExecutionSecurityContextReferenceV1 {
		return &runtimev1.ExecutionSecurityContextReferenceV1{
			ContentEntryId: securitycontext.ContentIDPrefix + "principal-1",
			Digest:         &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]},
			SchemaRevision: securitycontext.SchemaRevisionV1,
		}
	}
	principal, err = sources.RunPrincipal(ctx, &runtimev1.WorkerCommandV1{PrincipalRef: "31", SecurityContextRef: reference(digest)})
	if err != nil || principal.UserID != 44 || principal.MandateID != "mandate-1" || !principal.Governed ||
		!slices.Equal(principal.FrozenCredentialReferences, []string{principalRefLive}) {
		t.Fatalf("mandate principal = %+v, err = %v", principal, err)
	}
	wrong := digest
	wrong[0] ^= 0xff
	if _, err := sources.RunPrincipal(ctx, &runtimev1.WorkerCommandV1{SecurityContextRef: reference(wrong)}); !errors.Is(err, ErrPrincipalSecurityContextInvalid) {
		t.Fatalf("a context that does not match its reference: %v", err)
	}
}

package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// Issue #1119, CI-only: a governed agent with exact references and grants
// resolves them through the real credential grant readers, freezes each
// reference into its toolkit's settings, and the admission transaction
// writes the list into the run's security context. A reference of another
// tenant is refused.
const (
	resolutionExternal  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b01"
	resolutionBinding   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b02"
	resolutionTarget    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b03"
	resolutionReference = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b04"
	resolutionGrant     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b05"
	resolutionForeignT  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b06"
	resolutionForeignR  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b07"
)

type storedToolkits map[int64]map[string]any

func (s storedToolkits) StoredToolkit(_ context.Context, _ int64, id int64) (string, map[string]any, bool, error) {
	settings, ok := s[id]
	return "jira", settings, ok, nil
}

func TestPostgresGovernedCredentialResolutionFreezesTheList(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-resolution")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	stored := storedToolkits{
		21: {"jira_configuration": map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": resolutionReference}},
		22: {"jira_configuration": map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": resolutionForeignR}},
	}
	digest, err := credentialgrant.ToolkitDigest("jira", stored[21])
	if err != nil {
		t.Fatal(err)
	}
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-r', 'active', '100', '200')`, resolutionExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		resolutionBinding, bytes.Repeat([]byte{9}, 32), resolutionExternal, revision.ID)
	exec(`INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity)
VALUES ($1::uuid, 7, 'jira', 'https://jira.example.test'), ($2::uuid, 8, 'jira', 'https://jira.example.test')`,
		resolutionTarget, resolutionForeignT)
	exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', 'p7/jira', 'jira', $2::uuid, 3, 'active'),
       ($3::uuid, 8, 'static_secret', 'pgvault', 'p8/jira', 'jira', $4::uuid, 1, 'active')`,
		resolutionReference, resolutionTarget, resolutionForeignR, resolutionForeignT)
	exec(`INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, 21, 7, $2)`, resolutionBinding, digest[:])
	exec(`
INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, epoch, proposed_by, approved_by, last_transition_by)
VALUES ($1::uuid, 7, $2::uuid, 21, $3::uuid, $4::uuid, ARRAY['use'], ARRAY['jira.issue.read'], 'active', 1, '100', '200', '200')`,
		resolutionGrant, resolutionBinding, resolutionReference, resolutionTarget)

	grants := NewCredentialGrants(pool, nil)
	resolver, err := configurationapp.NewGovernedReferenceResolver(grants, grants, grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := agentexecutionapp.NewGovernedCredentials(resolver, NewAgentIdentityBindings(pool, nil), stored, NewAgentAdmissionReplays(pool))
	if err != nil {
		t.Fatal(err)
	}
	submit := func(toolkitID int) agentexecutionapp.SubmitRequest {
		application, _ := json.Marshal(map[string]any{"version_details": map[string]any{"tools": []any{
			map[string]any{"id": toolkitID, "type": "jira", "settings": map[string]any{"jira_configuration": map[string]any{"base_url": "https://jira.example.test"}}},
		}}})
		return agentexecutionapp.SubmitRequest{
			Identity:     executionapp.AdmissionIdentity{TenantID: "t", ResourceProjectID: "7", ProjectionProjectID: "7", ActorID: "5"},
			CapabilityID: executiondomain.AgentApplicationCapability,
			Input:        &runtimev1.AgentExecutionInputV1{Application: application},
			CurrentTurn:  &agentexecutionapp.CurrentApplicationTurn{ApplicationID: 3, ApplicationVersionID: 4},
		}
	}

	input, freeze, err := credentials.Freeze(ctx, submit(21))
	if err != nil {
		t.Fatal(err)
	}
	var frozen struct {
		VersionDetails struct {
			Tools []struct {
				Settings map[string]any `json:"settings"`
			} `json:"tools"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(input.GetApplication(), &frozen); err != nil || len(frozen.VersionDetails.Tools) != 1 ||
		frozen.VersionDetails.Tools[0].Settings["credential_reference_id"] != resolutionReference {
		t.Fatalf("frozen settings %s err=%v", input.GetApplication(), err)
	}
	want := agentexecutionapp.GovernedCredentialReference{
		ToolkitID: 21, CredentialReferenceID: resolutionReference, TargetBindingID: resolutionTarget, Generation: 3, ToolkitDigest: digest,
	}
	if freeze == nil || freeze.AgentBindingID != resolutionBinding || len(freeze.References) != 1 || freeze.References[0] != want {
		t.Fatalf("freeze %+v", freeze)
	}

	// The admission transaction writes the list into the security context.
	exec(`
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest)
VALUES ('resolution-1', 'elitea.credential-profile.governed.v1', $1, $2), ('resolution-2', 'elitea.credential-profile.governed.v1', $1, $2)`,
		revision.ID, revision.Digest[:])
	record := func(executionID string, freeze *agentexecutionapp.GovernedCredentialFreeze) (securitycontext.Reference, error) {
		var ref securitycontext.Reference
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			ref, err = recordSecurityContext(ctx, tx, pgxRows(tx), executionID, securityContextFacts{
				tenantProjectID: 7, actorID: "5", applicationID: 3, applicationVersionID: 4, admittedAt: time.Now().UTC(), governed: freeze,
			})
			return err
		})
		return ref, err
	}
	ref, err := record("resolution-1", freeze)
	if err != nil {
		t.Fatal(err)
	}
	var bytesStored []byte
	if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = 'resolution-1'`).Scan(&bytesStored); err != nil {
		t.Fatal(err)
	}
	decoded, err := securitycontext.Verify(bytesStored, ref)
	if err != nil || decoded.GetApplicationId() != 3 || decoded.GetApplicationVersionId() != 4 {
		t.Fatalf("context %+v err=%v", decoded, err)
	}
	if got, ok := securitycontext.CredentialReferenceFor(decoded, 21); !ok || got.CredentialReferenceID != resolutionReference ||
		got.TargetBindingID != resolutionTarget || got.Generation != 3 || got.ToolkitDigest != digest {
		t.Fatalf("frozen reference %+v %v", got, ok)
	}

	// A freeze resolved for another binding is refused by the transaction.
	moved := *freeze
	moved.AgentBindingID = resolutionGrant
	if _, err := record("resolution-2", &moved); !errors.Is(err, ErrGovernedBindingChanged) {
		t.Fatalf("binding changed: %v", err)
	}

	// A reference of another tenant is not found: nothing is looked up
	// outside the run's tenant project.
	if _, _, err := credentials.Freeze(ctx, submit(22)); !errors.Is(err, configurationapp.ErrGovernedReferenceNotFound) {
		t.Fatalf("foreign reference: %v", err)
	}
}

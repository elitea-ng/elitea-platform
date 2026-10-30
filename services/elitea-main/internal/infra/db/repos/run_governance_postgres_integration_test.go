package repos

import (
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// #1537 review item 3: the freeze's governance reader answers a
// continuation scope from its governed pin, and a parent execution from its
// stored security context. CI-only: skipped without the integration
// database.
func TestRunGovernance_ReadsPinsAndSecurityContexts(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	governance, err := NewRunGovernance(pool)
	if err != nil {
		t.Fatal(err)
	}
	scope := agentexecutionapp.ContinuationScope("conversation", "response")
	if governed, err := governance.GovernedContinuationScope(t.Context(), scope); err != nil || governed {
		t.Fatalf("an unpinned scope: governed=%v err=%v", governed, err)
	}
	credentials, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, t.Context(), credentials, "it-run-governance")
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest, continuation_scope)
VALUES ('exec-governed', 'elitea.credential-profile.governed.v1', $2, $3, $1)`, scope, revision.ID, revision.Digest[:]); err != nil {
		t.Fatal(err)
	}
	if governed, err := governance.GovernedContinuationScope(t.Context(), scope); err != nil || !governed {
		t.Fatalf("a governed scope: governed=%v err=%v", governed, err)
	}
	if governed, err := governance.GovernedExecution(t.Context(), "exec-none"); err != nil || governed {
		t.Fatalf("an execution without a security context: governed=%v err=%v", governed, err)
	}
}

package repos

// The approval policy sources (ADR-0041 decision 5, issue 1498) against a
// REAL migrated database: the root agent's risk tier from its registry row,
// and the approval_policy section's Stored gate (absent until an
// administrator saves it). It SKIPS without ELITEA_TEST_DATABASE_URL; each
// case asserts rows a skip could not have produced.

import (
	"context"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestWorkerApprovalPolicySourcesRead(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	// No registry row is the empty tier (read as low).
	tier, err := sources.AgentRiskTier(ctx, 1, 31)
	if err != nil || tier != "" {
		t.Fatalf("no row: %q %v", tier, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, risk_tier)
VALUES (1, 31, 'critical'), (1, 32, 'low')`); err != nil {
		t.Fatalf("seed the registry: %v", err)
	}
	for application, want := range map[int64]string{31: "critical", 32: "low", 33: ""} {
		tier, err := sources.AgentRiskTier(ctx, 1, application)
		if err != nil || tier != want {
			t.Fatalf("application %d: %q %v, want %q", application, tier, err, want)
		}
	}
	if tier, err := sources.AgentRiskTier(ctx, 2, 31); err != nil || tier != "" {
		t.Fatalf("another project read the row: %q %v", tier, err)
	}

	// The section: never saved is the ADR default and not Stored, so no
	// command carries field 68.
	settings, err := platformconfig.LoadApprovalPolicySettings(ctx, pool)
	if err != nil || settings.Stored ||
		!slices.Equal(settings.Policy.CategoriesFor(agentlifecycle.TierHigh),
			[]toolcategory.Category{toolcategory.Delete, toolcategory.ExternalSend, toolcategory.Payment}) {
		t.Fatalf("no table: %+v %v", settings, err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`); err != nil {
		t.Fatal(err)
	}
	settings, err = platformconfig.LoadApprovalPolicySettings(ctx, pool)
	if err != nil || settings.Stored {
		t.Fatalf("never saved: %+v %v", settings, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at) VALUES
    ('approval_policy', 'low_approval_categories', '["code_execution"]', now() AT TIME ZONE 'UTC'),
    ('approval_policy', 'require_hitl_tiers', '["high", "critical"]', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("seed the platform section: %v", err)
	}
	settings, err = platformconfig.LoadApprovalPolicySettings(ctx, pool)
	if err != nil || !settings.Stored ||
		!slices.Equal(settings.Policy.CategoriesFor(agentlifecycle.TierLow), []toolcategory.Category{toolcategory.CodeExecution}) ||
		!settings.Policy.RequiresHITL(agentlifecycle.TierCritical) || settings.Policy.RequiresHITL(agentlifecycle.TierMedium) ||
		len(settings.Policy.CategoriesFor(agentlifecycle.TierHigh)) != 3 {
		t.Fatalf("saved: %+v %v", settings, err)
	}
}

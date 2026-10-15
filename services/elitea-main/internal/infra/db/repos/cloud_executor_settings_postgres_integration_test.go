package repos

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type devicePolicy struct{ cloudHandoff bool }

func (d devicePolicy) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.LocalWork.CloudHandoff = d.cloudHandoff
	return policy, nil
}

// The effective policy over its real layers: the platform section, the
// project's cloud_executor configuration row and the device switch.
func TestPostgresCloudExecutorEffectivePolicy(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	// centry.platform_config comes from the bootstrap schema
	// (internal/infra/db/migrations/001_initial.sql), not the ledgered corpus.
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`); err != nil {
		t.Fatal(err)
	}
	repo, err := NewCloudExecutorSettingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := workspacehandoff.NewPolicyService(repo, repo, devicePolicy{cloudHandoff: true}, true,
		platformconfig.CloudExecutorResources{})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing saved: off.
	if got, err := policy.Effective(ctx, 1); err != nil || got.Enabled {
		t.Fatalf("defaults = %+v, %v; want off", got, err)
	}
	for key, value := range map[string]string{"enabled": "true", "max_segment_mb": "100", "network_allowlist": `["github.com","pypi.org"]`} {
		if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value) VALUES ('cloud_executor', $1, $2::jsonb)`, key, value); err != nil {
			t.Fatalf("seed platform %s: %v", key, err)
		}
	}
	got, err := policy.Effective(ctx, 1)
	if err != nil || !got.Enabled || got.Limits.MaxSegmentBytes != 100<<20 || len(got.NetworkAllowlist) != 2 {
		t.Fatalf("platform only = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source)
VALUES (gen_random_uuid(), 1, 'Cloud executor', 'cloud_executor', 'cloud_executor', 'project_settings',
        '{"shell": false, "network_allowlist": ["pypi.org"], "max_segment_mb": 500, "max_project_workspace_gb": 3}'::jsonb,
        '{}'::jsonb, false, true, 'user')`); err != nil {
		t.Fatal(err)
	}
	got, err = policy.Effective(ctx, 1)
	if err != nil || !got.Enabled || got.Shell || !got.Network || len(got.NetworkAllowlist) != 1 ||
		got.NetworkAllowlist[0] != "pypi.org" || got.Limits.MaxSegmentBytes != 100<<20 ||
		got.Limits.MaxProjectWorkspaceBytes != 3<<30 {
		t.Fatalf("with the project setting = %+v, %v", got, err)
	}
	off, _ := workspacehandoff.NewPolicyService(repo, repo, devicePolicy{cloudHandoff: false}, true, platformconfig.CloudExecutorResources{})
	if got, _ := off.Effective(ctx, 1); got.Enabled {
		t.Fatalf("the device switch off left the executor on")
	}
	unencrypted, _ := workspacehandoff.NewPolicyService(repo, repo, devicePolicy{cloudHandoff: true}, false, platformconfig.CloudExecutorResources{})
	if got, _ := unencrypted.Effective(ctx, 1); got.Enabled {
		t.Fatalf("an unconfirmed bucket encryption left the executor on")
	}
}

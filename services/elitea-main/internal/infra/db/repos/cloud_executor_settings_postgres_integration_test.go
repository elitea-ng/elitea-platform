package repos

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type devicePolicy struct{ cloudHandoff bool }

func (d devicePolicy) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.LocalWork.CloudHandoff = d.cloudHandoff
	policy.LocalWork.PathDeny = []string{"**/.env.production"}
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
INSERT INTO centry.platform_config (section, key, value, updated_at) VALUES ('cloud_executor', $1, $2::jsonb, now() AT TIME ZONE 'UTC')`, key, value); err != nil {
			t.Fatalf("seed platform %s: %v", key, err)
		}
	}
	// The upgrade rule: the platform on and its project default never saved
	// (a deployment from before the key) keeps a project that set nothing on.
	got, err := policy.Effective(ctx, 1)
	if err != nil || !got.Enabled {
		t.Fatalf("platform on, default never saved = %+v, %v; want on", got, err)
	}
	// A saved project default wins over that: off.
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('cloud_executor', 'project_default_enabled', 'false'::jsonb, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	got, err = policy.Effective(ctx, 1)
	if err != nil || got.Enabled || got.DisabledReason != workspacehandoff.DisabledProjectOff {
		t.Fatalf("platform on, default saved off = %+v, %v; want project_off", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE centry.platform_config SET value = 'true'::jsonb WHERE section = 'cloud_executor' AND key = 'project_default_enabled'`); err != nil {
		t.Fatal(err)
	}
	got, err = policy.Effective(ctx, 1)
	if err != nil || !got.Enabled || got.Limits.MaxSegmentBytes != 100<<20 || len(got.NetworkAllowlist) != 2 {
		t.Fatalf("platform only = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, created_at)
VALUES (gen_random_uuid(), 1, 'Cloud executor', 'cloud_executor', 'cloud_executor', 'project_settings',
        '{"shell": false, "network_allowlist": ["pypi.org"], "max_segment_mb": 500, "max_project_workspace_gb": 3,
          "exclusions_add": ["secrets/**"], "allow_user_include": false, "env_harvest": true, "env_deny_add": ["AWS_*"]}'::jsonb,
        '{}'::jsonb, false, true, 'user', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	got, err = policy.Effective(ctx, 1)
	if err != nil || !got.Enabled || got.Shell || !got.Network || len(got.NetworkAllowlist) != 1 ||
		got.NetworkAllowlist[0] != "pypi.org" || got.Limits.MaxSegmentBytes != 100<<20 ||
		got.Limits.MaxProjectWorkspaceBytes != 3<<30 {
		t.Fatalf("with the project setting = %+v, %v", got, err)
	}
	// The amendment's layers: the project adds patterns and turns include
	// off; it cannot turn the harvest on under a ceiling that has it off; the
	// device's path_deny is carried through.
	if got.Exclusions[0] != ".env" || got.Exclusions[len(got.Exclusions)-1] != "secrets/**" ||
		got.AllowUserInclude || got.EnvHarvest || got.EnvDeny[len(got.EnvDeny)-1] != "AWS_*" ||
		len(got.PathDeny) != 1 || got.PathDeny[0] != "**/.env.production" {
		t.Fatalf("amendment layers = %+v", got)
	}
	// The project's addition shows up only in exclusions_project; the
	// platform list is unchanged by it.
	if len(got.ExclusionsProject) != 1 || got.ExclusionsProject[0] != "secrets/**" ||
		slices.Contains(got.ExclusionsPlatform, "secrets/**") ||
		!slices.Equal(got.ExclusionsPlatform, platformconfig.DefaultCloudExecutorExclusions()) {
		t.Fatalf("exclusions split = platform %v, project %v", got.ExclusionsPlatform, got.ExclusionsProject)
	}
	// An explicit off in the row wins over the default that is on.
	if _, err := pool.Exec(ctx, `UPDATE p_1.configuration SET data = data || '{"enabled": false}'::jsonb WHERE type = 'cloud_executor'`); err != nil {
		t.Fatal(err)
	}
	if got, err := policy.Effective(ctx, 1); err != nil || got.Enabled || got.DisabledReason != workspacehandoff.DisabledProjectOff {
		t.Fatalf("explicit off = %+v, %v", got, err)
	}
	// An explicit on wins over a default that is off, and the platform's off
	// wins over it.
	if _, err := pool.Exec(ctx, `UPDATE p_1.configuration SET data = data || '{"enabled": true}'::jsonb WHERE type = 'cloud_executor'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE centry.platform_config SET value = 'false'::jsonb WHERE section = 'cloud_executor' AND key = 'project_default_enabled'`); err != nil {
		t.Fatal(err)
	}
	if got, err := policy.Effective(ctx, 1); err != nil || !got.Enabled {
		t.Fatalf("explicit on under a default off = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE centry.platform_config SET value = 'false'::jsonb WHERE section = 'cloud_executor' AND key = 'enabled'`); err != nil {
		t.Fatal(err)
	}
	if got, err := policy.Effective(ctx, 1); err != nil || got.Enabled || got.DisabledReason != workspacehandoff.DisabledPlatformOff {
		t.Fatalf("explicit on under the platform off = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE centry.platform_config SET value = 'true'::jsonb WHERE section = 'cloud_executor' AND key = 'enabled'`); err != nil {
		t.Fatal(err)
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

// The project switch's write is a configuration mutation: merge, remove,
// create once, one lifecycle outbox event per write with its actor and the
// next revision, a generated title that cannot clash with an existing one,
// and an unknown project refused. Project 1 has no vault (a fresh install
// provisions none), which the secret-free mutation must not need.
func TestPostgresCloudExecutorProjectSwitchWrite(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.secrets_key (id TEXT PRIMARY KEY, data BYTEA);
CREATE TABLE IF NOT EXISTS centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA);`); err != nil {
		t.Fatal(err)
	}
	// A row of another type already holds the plain title `cloud_executor`,
	// and elitea_title is UNIQUE across the table.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, created_at)
VALUES (gen_random_uuid(), 1, 'Icon', 'cloud_executor', 'project_icon', 'project_settings', '{"url": "x"}'::jsonb,
        '{}'::jsonb, false, true, 'user', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	repo, err := NewCloudExecutorSettingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	const actor = 77
	on, off := true, false
	// nil without a row writes nothing, not even an event.
	if err := repo.SetCloudExecutorProjectEnabled(ctx, 1, actor, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.CloudExecutorSetting(ctx, 1); err != nil || found {
		t.Fatalf("nil created a row: found %v, %v", found, err)
	}
	// No acting user, no write.
	if err := repo.SetCloudExecutorProjectEnabled(ctx, 1, 0, &on); err == nil {
		t.Fatal("a write without an actor was accepted")
	}
	// Concurrent first writes create exactly one row.
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { errs <- repo.SetCloudExecutorProjectEnabled(ctx, 1, actor, &on) }()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	var title, rowUUID, source string
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), elitea_title, uuid::text, source FROM p_1.configuration
WHERE type = 'cloud_executor'`).Scan(&rows, &title, &rowUUID, &source); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v; want one", rows, err)
	}
	if !strings.HasPrefix(title, "cloud_executor_") || len(title) != len("cloud_executor_")+12 || source != "user" {
		t.Fatalf("new row title %q, source %q; want a generated cloud_executor_<12 hex> by the user", title, source)
	}
	// One event per write: created, then seven updates, revisions 1..8, all
	// by the actor.
	events := func() (count int, maxRevision int64, operations []string) {
		t.Helper()
		result, err := pool.Query(ctx, `SELECT revision, operation, actor_id FROM elitea_runtime.configuration_lifecycle_outbox
WHERE resource_project_id = 1 AND configuration_uuid = $1::uuid ORDER BY revision`, rowUUID)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		for result.Next() {
			var revision, actorID int64
			var operation string
			if err := result.Scan(&revision, &operation, &actorID); err != nil {
				t.Fatal(err)
			}
			count++
			if revision != int64(count) || actorID != actor {
				t.Fatalf("event %d: revision %d, actor %d; want %d and %d", count, revision, actorID, count, actor)
			}
			maxRevision = revision
			operations = append(operations, operation)
		}
		return count, maxRevision, operations
	}
	if count, _, operations := events(); count != 8 || operations[0] != "configuration_created" || operations[7] != "configuration_updated" {
		t.Fatalf("events = %d %v; want a create and seven updates", count, operations)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.configuration SET data = data || '{"network": false}'::jsonb WHERE type = 'cloud_executor'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetCloudExecutorProjectEnabled(ctx, 1, actor, &off); err != nil {
		t.Fatal(err)
	}
	setting, _, err := repo.CloudExecutorSetting(ctx, 1)
	if err != nil || setting.Enabled == nil || *setting.Enabled || setting.Network == nil || *setting.Network {
		t.Fatalf("after off = %+v, %v; want enabled false and network kept", setting, err)
	}
	var snapshot []byte
	if err := pool.QueryRow(ctx, `SELECT sanitized_snapshot FROM elitea_runtime.configuration_lifecycle_outbox
WHERE configuration_uuid = $1::uuid AND revision = 9`, rowUUID).Scan(&snapshot); err != nil ||
		!strings.Contains(string(snapshot), `"enabled":false`) || !strings.Contains(string(snapshot), `"enabled":true`) {
		t.Fatalf("the off event's snapshot = %s, %v; want before on and after off", snapshot, err)
	}
	if err := repo.SetCloudExecutorProjectEnabled(ctx, 1, actor, nil); err != nil {
		t.Fatal(err)
	}
	setting, found, err := repo.CloudExecutorSetting(ctx, 1)
	if err != nil || !found || setting.Enabled != nil || setting.Network == nil {
		t.Fatalf("after nil = %+v, %v, %v; want enabled unset and the row kept", setting, found, err)
	}
	if count, maxRevision, _ := events(); count != 10 || maxRevision != 10 {
		t.Fatalf("events after off and unset = %d (revision %d); want 10", count, maxRevision)
	}
	// The other type's row is untouched.
	var icon []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM p_1.configuration WHERE elitea_title = 'cloud_executor'`).Scan(&icon); err != nil ||
		string(icon) != `{"url": "x"}` {
		t.Fatalf("the project_icon row = %s, %v", icon, err)
	}
	// A project with no tenant schema.
	if err := repo.SetCloudExecutorProjectEnabled(ctx, 424242, actor, &on); !errors.Is(err, workspacehandoff.ErrProjectNotFound) {
		t.Fatalf("unknown project write = %v", err)
	}
	if _, _, err := repo.CloudExecutorSetting(ctx, 424242); !errors.Is(err, workspacehandoff.ErrProjectNotFound) {
		t.Fatalf("unknown project read = %v", err)
	}
}

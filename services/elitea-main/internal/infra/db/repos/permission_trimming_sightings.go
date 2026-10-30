package repos

// The permission trimming routine dates an egress allowlist entry by its
// first sighting (elitea_lifecycle.permission_trimming_observed): an
// allowlist has no per-entry timestamp. Every path that takes an entry off
// an allowlist forgets the entry's sighting in its own transaction, so an
// entry added back starts its window over instead of inheriting the old
// age: the Configuration route and the direct row writers (both append the
// configuration lifecycle event, which is where this hooks in) for a
// project's list, and the Configuration page's platform save for the
// platform's.

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// sightingsExec is a transaction (pgx.Tx or the mutation store's executor).
type sightingsExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

const workerEgressConfigurationType = "worker_egress"

// sightingsTable reports whether the sightings table exists: a database
// without migration 0208 (the configuration tests' 001_initial.sql-only
// schema) has none to forget. A SELECT's command tag carries its row count.
func sightingsTable(ctx context.Context, exec sightingsExec) (bool, error) {
	tag, err := exec.Exec(ctx, `
SELECT 1 WHERE to_regclass('elitea_lifecycle.permission_trimming_observed') IS NOT NULL`)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// allowlistOf reads a worker_egress snapshot's entries.
func allowlistOf(snapshot *configurationapp.CurrentConfigurationLifecycleSnapshot) []string {
	if snapshot == nil || snapshot.Data == nil {
		return nil
	}
	var out []string
	switch list := snapshot.Data["allowlist"].(type) {
	case []any:
		for _, item := range list {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
	case []string:
		out = append(out, list...)
	}
	return out
}

// forgetRemovedProjectEgressSightings forgets the sightings of the entries a
// project worker_egress write took off the list (every entry when the row
// was deleted). A write to any other configuration type is not one.
func forgetRemovedProjectEgressSightings(
	ctx context.Context, exec sightingsExec,
	before, after *configurationapp.CurrentConfigurationLifecycleSnapshot,
) error {
	if before == nil || before.Type != workerEgressConfigurationType || before.ProjectID <= 0 {
		return nil
	}
	kept := map[string]bool{}
	if after != nil {
		for _, entry := range allowlistOf(after) {
			kept[entry] = true
		}
	}
	var removed []string
	for _, entry := range allowlistOf(before) {
		if !kept[entry] {
			removed = append(removed, "project:"+entry)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if exists, err := sightingsTable(ctx, exec); err != nil || !exists {
		return err
	}
	_, err := exec.Exec(ctx, `
DELETE FROM elitea_lifecycle.permission_trimming_observed
WHERE tenant_project_id = $1 AND application_id = 0 AND item_kind = 'egress_host' AND item_key = ANY($2::text[])`,
		int64(before.ProjectID), removed)
	return err
}

// ForgetPlatformEgressSightingsTx forgets the sightings of the platform
// allowlist entries not in allowlist, in the caller's transaction. The
// platform Configuration page's save of the worker_egress section calls it
// with the list it stores.
func ForgetPlatformEgressSightingsTx(ctx context.Context, exec sightingsExec, allowlist []string) error {
	if exists, err := sightingsTable(ctx, exec); err != nil || !exists {
		return err
	}
	keep := make([]string, 0, len(allowlist))
	for _, entry := range allowlist {
		keep = append(keep, "platform:"+entry)
	}
	_, err := exec.Exec(ctx, `
DELETE FROM elitea_lifecycle.permission_trimming_observed
WHERE tenant_project_id = 0 AND application_id = 0 AND item_kind = 'egress_host'
  AND NOT (item_key = ANY($1::text[]))`, keep)
	return err
}

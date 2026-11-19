package marketplace

import (
	"context"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// dismissalGrace keeps a fresh hire out of the reconcile: a hire is recorded in
// the transaction that creates the entity, but a slow commit must never read as
// a removal.
const dismissalGrace = "10 minutes"

// ReconcileDismissals marks as dismissed every hire whose agent or skill no
// longer exists in its project (#2089). MarkDismissed covers the one removal
// path that knows about hires (an agent's Delete); the others (a skill delete,
// the workforce ladder, a catalogue-twin delete, the undo of an automatic hire,
// a project delete) cannot all be taught about the Marketplace, so the daily
// pass closes the gap from the tenant tables: an install whose entity row is
// gone, or whose project schema is gone, is dismissed with no actor. A hire the
// person removed earlier keeps its own dismissal time.
func ReconcileDismissals(ctx context.Context, db DB) (int64, error) {
	rows, err := db.Query(ctx, `
SELECT DISTINCT project_id, entity_kind
FROM elitea_marketplace.installs
WHERE dismissed_at IS NULL AND created_at < clock_timestamp() - interval '`+dismissalGrace+`'`)
	if err != nil {
		return 0, fmt.Errorf("marketplace: read the live hires: %w", err)
	}
	type scope struct {
		project int64
		kind    string
	}
	var scopes []scope
	for rows.Next() {
		var s scope
		if err := rows.Scan(&s.project, &s.kind); err != nil {
			rows.Close()
			return 0, fmt.Errorf("marketplace: read a live hire: %w", err)
		}
		scopes = append(scopes, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("marketplace: read the live hires: %w", err)
	}
	var dismissed int64
	for _, s := range scopes {
		table := "applications"
		if s.kind == KindSkill {
			table = "skills"
		}
		schema, err := tenantschema.QuoteInt(s.project)
		if err != nil {
			continue
		}
		var present bool
		if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&present); err != nil {
			return dismissed, fmt.Errorf("marketplace: look for the %s table of project %d: %w", table, s.project, err)
		}
		gone := ""
		if present {
			gone = fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM %s.%s t WHERE t.id = i.entity_id)`, schema, table)
		}
		tag, err := db.Exec(ctx, `
UPDATE elitea_marketplace.installs i
SET dismissed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE i.project_id = $1 AND i.entity_kind = $2 AND i.dismissed_at IS NULL
  AND i.created_at < clock_timestamp() - interval '`+dismissalGrace+`'`+gone, s.project, s.kind)
		if err != nil {
			return dismissed, fmt.Errorf("marketplace: record the removed hires of project %d: %w", s.project, err)
		}
		dismissed += tag.RowsAffected()
	}
	return dismissed, nil
}

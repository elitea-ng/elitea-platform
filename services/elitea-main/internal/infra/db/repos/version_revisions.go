package repos

// Draft revisions of an agent version (ADR-0040 decision 2, issue #1492,
// shared 0198 elitea_lifecycle.version_revisions).
//
// Every save of a version through ApplicationsRepo (UpdateVersion and
// UpdateWithVersion) appends one revision in the same transaction as the
// save: the version's saved fields after the save, the saving user, the time,
// and a JSON diff against the previous revision. A version saved for the
// first time after 0198 first gets revision 1 of kind `baseline`: the state
// the save found, so the first diff has something to compare with. A save
// that changes nothing appends nothing.
//
// The snapshot holds the version row's own fields. It does not hold the
// toolkit and skill attachments: other routes write those, and a release
// pins them when the version is published.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
)

// versionRevisionStateColumns is the snapshot of one version row. `v` is the
// version alias.
const versionRevisionStateColumns = `jsonb_build_object(
	'name', v.name,
	'status', v.status,
	'agent_type', COALESCE(v.agent_type, ''),
	'instructions', COALESCE(v.instructions, ''),
	'welcome_message', COALESCE(v.welcome_message, ''),
	'llm_settings', COALESCE(NULLIF(v.llm_settings::jsonb, 'null'::jsonb), '{}'::jsonb),
	'conversation_starters', COALESCE(NULLIF(v.conversation_starters::jsonb, 'null'::jsonb), '[]'::jsonb),
	'meta', COALESCE(NULLIF(v.meta::jsonb, 'null'::jsonb), '{}'::jsonb),
	'pipeline_settings', COALESCE(NULLIF(v.pipeline_settings::jsonb, 'null'::jsonb), '{}'::jsonb)
)::text`

// errVersionRevisionStateMissing is a version the revision read found no row
// for.
var errVersionRevisionStateMissing = errors.New("version revision: the version does not exist")

// readVersionRevisionState reads the snapshot of one version. lock takes the
// row lock, so the state read before a save is the state the save changes.
func readVersionRevisionState(ctx context.Context, q relationReader, schema, applicationID, versionID string, lock bool) (map[string]any, error) {
	query := fmt.Sprintf(`SELECT `+versionRevisionStateColumns+`
FROM %s.application_versions v
WHERE v.application_id = $1 AND v.id = $2`, schema)
	if lock {
		query += ` FOR UPDATE`
	}
	var raw string
	err := q.QueryRow(ctx, query, applicationID, versionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errVersionRevisionStateMissing
	}
	if err != nil {
		return nil, fmt.Errorf("version revision: read the version: %w", err)
	}
	state, err := agentrelease.DecodeObject([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("version revision: decode the version: %w", err)
	}
	return state, nil
}

// appendVersionRevision appends the revision of one save. before is the
// state the save found (read under the row lock), after the state it left.
// The caller holds the version's row lock in the same transaction, so two
// saves of one version append their revisions one after the other.
func appendVersionRevision(ctx context.Context, q relationReader, projectID, applicationID, versionID int64, before, after map[string]any, actorID int64) error {
	var (
		last     int
		snapshot string
	)
	err := q.QueryRow(ctx, `
SELECT revision, snapshot::text
FROM elitea_lifecycle.version_revisions
WHERE tenant_project_id = $1 AND application_version_id = $2
ORDER BY revision DESC
LIMIT 1`, projectID, versionID).Scan(&last, &snapshot)
	previous := before
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The first save since 0198: record what it found as revision 1.
		baseline, encodeErr := json.Marshal(before)
		if encodeErr != nil {
			return fmt.Errorf("version revision: encode the baseline: %w", encodeErr)
		}
		var id int64
		if err := q.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.version_revisions
	(tenant_project_id, application_id, application_version_id, revision, kind, snapshot, diff)
VALUES ($1, $2, $3, 1, 'baseline', $4::jsonb, '[]'::jsonb)
RETURNING id`, projectID, applicationID, versionID, string(baseline)).Scan(&id); err != nil {
			return fmt.Errorf("version revision: insert the baseline: %w", err)
		}
		last = 1
	case err != nil:
		return fmt.Errorf("version revision: read the last revision: %w", err)
	default:
		if previous, err = agentrelease.DecodeObject([]byte(snapshot)); err != nil {
			return fmt.Errorf("version revision: decode the last revision: %w", err)
		}
	}
	changes := agentrelease.Diff(previous, after)
	if len(changes) == 0 {
		return nil
	}
	encodedSnapshot, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("version revision: encode the snapshot: %w", err)
	}
	encodedDiff, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("version revision: encode the diff: %w", err)
	}
	var actor any
	if actorID > 0 {
		actor = actorID
	}
	var id int64
	if err := q.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.version_revisions
	(tenant_project_id, application_id, application_version_id, revision, kind, actor_id, snapshot, diff)
VALUES ($1, $2, $3, $4, 'save', $5, $6::jsonb, $7::jsonb)
RETURNING id`, projectID, applicationID, versionID, last+1, actor, string(encodedSnapshot), string(encodedDiff)).Scan(&id); err != nil {
		return fmt.Errorf("version revision: insert the revision: %w", err)
	}
	return nil
}

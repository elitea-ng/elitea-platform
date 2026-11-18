package repos

// The notice of a refused or narrowed delegation (ADR-0044 Amendment 1,
// A1.2 rule 5; delegation epic 2156 story S5).
//
// The Rust worker streams `delegation_refused` and `delegation_narrowed` node
// events next to the callee's tool-call row. Main folds each accepted frame
// onto its answer's `meta.delegation_notices_v1`, in the transaction that
// accepts the frame, so a reloaded transcript shows the notice. The notice is
// display data: it is rebuilt into a closed, bounded shape, an entry that does
// not fit (or a frame without one) is ignored rather than refusing the frame,
// and a frame bound to another answer is refused as every projection of the
// run's response binding refuses it. No migration: the answer's meta already
// holds its display keys, and an answer keeps at most delegationnotice.MaxNotices.
//
// What the viewer may do about a notice is not stored: it is derived per
// viewer when the notice is served (delegationnotice.Stamp).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// projectDelegationNotice folds one accepted `delegation_refused` or
// `delegation_narrowed` frame onto its answer's `meta.delegation_notices_v1`.
// A frame of another type is not its business.
func projectDelegationNotice(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame) error {
	// Every accepted agent frame passes here: only one that names the notice
	// is decoded.
	if !bytes.Contains(frame.BrowserData, delegationnotice.FrameMarker) {
		return nil
	}
	var event struct {
		Type                string                     `json:"type"`
		StreamID            string                     `json:"stream_id"`
		MessageID           string                     `json:"message_id"`
		ExecutionGeneration string                     `json:"execution_generation"`
		SIOEvent            string                     `json:"sio_event"`
		ResponseMetadata    map[string]json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	if event.Type != delegationnotice.EventRefused && event.Type != delegationnotice.EventNarrowed {
		return nil
	}
	raw, ok := event.ResponseMetadata[delegationnotice.FrameKey]
	if !ok {
		return nil
	}
	notice, ok := delegationnotice.Sanitize(raw)
	if !ok {
		return nil
	}
	projectDatabaseID, valid := currentAgentDatabaseID(projectID)
	if !valid {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID ||
		event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return errors.New("delegation notice conflicts with immutable response binding")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	// The worker names the root caller by its runtime label ("elitea-agent"),
	// not by the agent: the stored notice carries the saved agent's own name
	// when its id is known, so the chat says which agent could not delegate.
	if callerID := notice.Edge.CallerID; callerID > 0 {
		var name string
		switch err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.applications WHERE id = $1`, schema), callerID).Scan(&name); {
		case err == nil:
			if clipped := strings.TrimSpace(name); clipped != "" {
				notice.Edge.CallerName = delegationnotice.ClipName(clipped)
			}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return fmt.Errorf("read the calling agent's name: %w", err)
		}
	}
	groupID, err := lockCurrentAgentMessageGroup(ctx, tx, schema, frame.Fence.ExecutionID, binding)
	if err != nil {
		return err
	}
	var stored []byte
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(meta -> '%s', '[]'::jsonb) FROM %s WHERE id = $1`,
		delegationnotice.MetaKey, schema+".chat_message_group"), groupID).Scan(&stored); err != nil {
		return fmt.Errorf("load delegation notices: %w", err)
	}
	existing := delegationnotice.Decode(stored)
	merged := delegationnotice.Merge(existing, notice)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode delegation notices: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s
SET meta = jsonb_set(COALESCE(meta, '{}'::jsonb), '{%s}', $2::jsonb, TRUE),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, schema+".chat_message_group", delegationnotice.MetaKey), groupID, encoded); err != nil {
		return fmt.Errorf("store delegation notices: %w", err)
	}
	return nil
}

// DelegationEditors names the people to ask about an agent's delegations: its
// registry owners (ADR-0040), else its owner. It implements
// delegationnotice.EditorLister.
type DelegationEditors struct {
	pool *pgxpool.Pool
}

// NewDelegationEditors builds the lister over the pool.
func NewDelegationEditors(pool *pgxpool.Pool) (*DelegationEditors, error) {
	if pool == nil {
		return nil, errors.New("delegation editors: a pool is required")
	}
	return &DelegationEditors{pool: pool}, nil
}

// EditorNames answers up to limit display names, registry owners first. A
// tenant without the registry (a fresh install that never ran the lifecycle
// migrations) falls back to the agent's owner.
func (e *DelegationEditors) EditorNames(ctx context.Context, projectID, applicationID int64, limit int) ([]string, error) {
	if projectID <= 0 || applicationID <= 0 || limit <= 0 {
		return nil, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	var owners []int64
	var business, technical *int64
	err = e.pool.QueryRow(ctx, `
SELECT business_owner_id, technical_owner_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&business, &technical)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		for _, id := range []*int64{business, technical} {
			if id != nil && *id > 0 {
				owners = append(owners, *id)
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
	case errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000"):
	default:
		return nil, fmt.Errorf("delegation editors: read the registry: %w", err)
	}
	if len(owners) == 0 {
		var owner *int64
		err := e.pool.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id::bigint FROM %s.applications WHERE id = $1`, schema),
			applicationID).Scan(&owner)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("delegation editors: read the owner: %w", err)
		}
		if owner != nil && *owner > 0 {
			owners = append(owners, *owner)
		}
	}
	if len(owners) == 0 {
		return nil, nil
	}
	rows, err := e.pool.Query(ctx, `
SELECT NULLIF(name, '')
FROM public.auth_core__user WHERE id = ANY($1::bigint[])
ORDER BY array_position($1::bigint[], id)`, owners)
	if err != nil {
		return nil, fmt.Errorf("delegation editors: read the names: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name *string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("delegation editors: scan a name: %w", err)
		}
		if name != nil && *name != "" && len(names) < limit {
			names = append(names, *name)
		}
	}
	return names, rows.Err()
}

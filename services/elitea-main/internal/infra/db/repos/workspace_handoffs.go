package repos

// The handoff state machine (ADR-0034 decision 7) over
// elitea_runtime.workspace_handoffs (shared/0167).
//
// Every ownership change is `UPDATE ... WHERE state = ANY($from) AND epoch =
// $epoch` (transition below). The request-driven operations first lock the row
// (FOR UPDATE) so a refusal can name its reason; the sweeper's timed
// transitions rely on the compare-and-set alone, so replicas never wait on
// each other.
//
// A send and a local turn are serialised by a transaction-level advisory lock
// on the thread (lockWorkspaceThread): the send takes it exclusively, a local
// turn's start and commit share it. Without it a local turn started while a
// send checks "no uncommitted local turn" could slip between the check and the
// new state.
//
// The system-authored event messages (decision 8) are written in the SAME
// transaction as the transition, into the tenant chat tables, by the tenant
// schema's `system` participant — never the user's participant, which would
// move the memory-recall boundary (MemoriesRepo.ResolveCurrentMemoryRecall).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// WorkspaceHandoffsRepository implements workspacehandoff.Repository.
type WorkspaceHandoffsRepository struct {
	pool *pgxpool.Pool
}

var _ workspacehandoff.Repository = (*WorkspaceHandoffsRepository)(nil)

func NewWorkspaceHandoffsRepository(pool *pgxpool.Pool) (*WorkspaceHandoffsRepository, error) {
	if pool == nil {
		return nil, errors.New("workspace handoffs database is required")
	}
	return &WorkspaceHandoffsRepository{pool: pool}, nil
}

// lockWorkspaceThread takes the thread's transaction-level advisory lock:
// exclusive for a send, shared for a local turn.
func lockWorkspaceThread(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string, shared bool) error {
	function := "pg_advisory_xact_lock"
	if shared {
		function = "pg_advisory_xact_lock_shared"
	}
	key := "elitea.workspace_handoff:" + strconv.FormatInt(projectID, 10) + ":" + conversationUUID
	if _, err := tx.Exec(ctx, `SELECT `+function+`(hashtextextended($1::text, 0))`, key); err != nil {
		return fmt.Errorf("workspace handoff: lock thread: %w", err)
	}
	return nil
}

// workspaceThreadState answers the thread's state under the shared thread lock
// (LOCAL when there is no row). The local turn fence reads it.
func workspaceThreadState(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string) (workspacestore.State, error) {
	if err := lockWorkspaceThread(ctx, tx, projectID, conversationUUID, true); err != nil {
		return "", err
	}
	var state string
	err := tx.QueryRow(ctx, `
SELECT state FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		projectID, conversationUUID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacestore.StateLocal, nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace handoff: read state: %w", err)
	}
	return workspacestore.State(state), nil
}

const handoffColumns = `project_id, conversation_uuid::text, handoff_id::text, state, epoch, reclaim_pending,
       reclaim_requested_at, owner_user_id, device_id, device_name, history_mode, has_upstream,
       base_sha, upstream_sha, handoff_sha, cloud_sha, stats, excluded, last_seq,
       COALESCE(running_execution_id, ''), COALESCE(running_response_message_id::text, ''), expires_at,
       state_changed_at, updated_at, delete_after, deleted_at, note`

type handoffRow struct {
	workspacehandoff.Handoff
	note string
}

func scanHandoff(row pgx.Row) (handoffRow, error) {
	var (
		out      handoffRow
		state    string
		stats    []byte
		excluded []byte
	)
	err := row.Scan(&out.ProjectID, &out.ConversationUUID, &out.HandoffID, &state, &out.Epoch, &out.ReclaimPending,
		&out.ReclaimRequestedAt, &out.OwnerUserID, &out.DeviceID, &out.DeviceName, &out.HistoryMode, &out.HasUpstream,
		&out.BaseSHA, &out.UpstreamSHA, &out.HandoffSHA, &out.CloudSHA, &stats, &excluded, &out.LastSeq,
		&out.RunningExecutionID, &out.RunningResponseID, &out.ExpiresAt,
		&out.StateChangedAt, &out.UpdatedAt, &out.DeleteAfter, &out.DeletedAt, &out.note)
	if err != nil {
		return handoffRow{}, err
	}
	out.State = workspacestore.State(state)
	out.Stats = json.RawMessage(stats)
	_ = json.Unmarshal(excluded, &out.Excluded)
	if out.Excluded == nil {
		out.Excluded = []string{}
	}
	return out, nil
}

func scanHandoffs(rows pgx.Rows) ([]workspacehandoff.Handoff, error) {
	defer rows.Close()
	out := []workspacehandoff.Handoff{}
	for rows.Next() {
		row, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row.Handoff)
	}
	return out, rows.Err()
}

func (r *WorkspaceHandoffsRepository) Read(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	row, err := scanHandoff(r.pool.QueryRow(ctx, `SELECT `+handoffColumns+`
FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacehandoff.Handoff{}, false, nil
	}
	if err != nil {
		return workspacehandoff.Handoff{}, false, fmt.Errorf("workspace handoff: read: %w", err)
	}
	return row.Handoff, true, nil
}

func lockHandoff(ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread) (handoffRow, bool, error) {
	row, err := scanHandoff(tx.QueryRow(ctx, `SELECT `+handoffColumns+`
FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid FOR UPDATE`,
		thread.ProjectID, thread.ConversationUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return handoffRow{}, false, nil
	}
	if err != nil {
		return handoffRow{}, false, fmt.Errorf("workspace handoff: lock: %w", err)
	}
	return row, true, nil
}

// transition is the one compare-and-set every ownership change goes through.
// `set` adds assignments; its placeholders start at $7.
func transition(
	ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread, from []workspacestore.State, epoch int64,
	to workspacestore.State, bump int64, set string, args ...any,
) (handoffRow, error) {
	states := make([]string, 0, len(from))
	for _, state := range from {
		states = append(states, string(state))
	}
	stateChanged := "state_changed_at = CASE WHEN state = $3 THEN state_changed_at ELSE clock_timestamp() END"
	if bump > 0 {
		stateChanged = "state_changed_at = clock_timestamp()"
	}
	query := `UPDATE elitea_runtime.workspace_handoffs
SET ` + stateChanged + `, state = $3, epoch = epoch + $4, updated_at = clock_timestamp()` + set + `
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND state = ANY($5::text[]) AND epoch = $6
RETURNING ` + handoffColumns
	all := append([]any{thread.ProjectID, thread.ConversationUUID, string(to), bump, states, epoch}, args...)
	row, err := scanHandoff(tx.QueryRow(ctx, query, all...))
	if errors.Is(err, pgx.ErrNoRows) {
		return handoffRow{}, workspacehandoff.ErrStaleEpoch
	}
	if err != nil {
		return handoffRow{}, fmt.Errorf("workspace handoff: transition to %s: %w", to, err)
	}
	return row, nil
}

func (r *WorkspaceHandoffsRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("workspace handoff: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workspace handoff: commit: %w", err)
	}
	return nil
}

func projectWorkspaceBytesExcept(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, thread workspacehandoff.Thread) (int64, error) {
	var used int64
	if err := q.QueryRow(ctx, `
SELECT COALESCE(sum(byte_length), 0)::bigint FROM elitea_runtime.workspace_segments
WHERE project_id = $1 AND superseded_at IS NULL AND conversation_uuid <> $2::uuid`,
		thread.ProjectID, thread.ConversationUUID).Scan(&used); err != nil {
		return 0, fmt.Errorf("workspace handoff: project bytes: %w", err)
	}
	return used, nil
}

func (r *WorkspaceHandoffsRepository) ProjectWorkspaceBytes(ctx context.Context, thread workspacehandoff.Thread) (int64, error) {
	return projectWorkspaceBytesExcept(ctx, r.pool, thread)
}

// Begin is the send (LOCAL → HANDING_OFF).
func (r *WorkspaceHandoffsRepository) Begin(ctx context.Context, record workspacehandoff.BeginRecord) (workspacehandoff.BeginResult, error) {
	schema, err := tenantSchema(strconv.FormatInt(record.Thread.ProjectID, 10))
	if err != nil {
		return workspacehandoff.BeginResult{}, workspacehandoff.ErrInvalid
	}
	var result workspacehandoff.BeginResult
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		thread := record.Thread
		if err := lockWorkspaceThread(ctx, tx, thread.ProjectID, thread.ConversationUUID, false); err != nil {
			return err
		}
		var (
			source string
			author int64
		)
		err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT source, author_id FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema), thread.ConversationUUID).
			Scan(&source, &author)
		if errors.Is(err, pgx.ErrNoRows) {
			return workspacehandoff.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("workspace handoff: read conversation: %w", err)
		}
		// Only the thread owner sends (decision 1).
		if author != record.ActorUserID {
			return workspacehandoff.ErrNotFound
		}
		if source != localWorkConversationSource {
			return workspacehandoff.ErrNotLocalWork
		}
		existing, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if found {
			switch {
			case existing.State.Cloud():
				return workspacehandoff.ErrExecutorIsCloud
			case existing.State != workspacestore.StateLocal:
				return workspacehandoff.ErrInProgress
			case existing.DeletedAt == nil && existing.DeleteAfter != nil && !existing.DeleteAfter.After(time.Now()):
				// The previous cloud copy is being erased: the erasure owns the
				// thread prefix until its listing is empty.
				return workspacehandoff.ErrInProgress
			}
		}
		var pending bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.local_turn_executions
               WHERE project_id = $1 AND conversation_uuid = $2::uuid
                 AND committed_at IS NULL AND expires_at > clock_timestamp())`,
			thread.ProjectID, thread.ConversationUUID).Scan(&pending); err != nil {
			return fmt.Errorf("workspace handoff: read local turns: %w", err)
		}
		if pending {
			return workspacehandoff.ErrLocalTurnInProgress
		}
		used, err := projectWorkspaceBytesExcept(ctx, tx, thread)
		if err != nil {
			return err
		}
		if used+record.DeclaredBytes > record.MaxProjectWorkspaceBytes {
			return workspacehandoff.ErrQuotaExceeded
		}
		result.ProjectBytesUsed = used
		deviceName := nativeDeviceName(ctx, tx, record.TokenID, record.ActorUserID)
		excluded, _ := json.Marshal(record.Excluded)
		handoffID := uuid.NewString()
		var row handoffRow
		if !found {
			row, err = scanHandoff(tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (
    project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id, device_id, device_name, token_id,
    history_mode, has_upstream, excluded, stats, note, expires_at
) VALUES ($1, $2::uuid, $3::uuid, 'HANDING_OFF', 1, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12,
          clock_timestamp() + make_interval(secs => $13))
RETURNING `+handoffColumns,
				thread.ProjectID, thread.ConversationUUID, handoffID, record.ActorUserID, record.DeviceID, deviceName,
				record.TokenID, record.HistoryMode, record.HasUpstream, string(excluded), string(record.Stats),
				record.Note, record.TTL.Seconds()))
			if err != nil {
				return fmt.Errorf("workspace handoff: insert: %w", err)
			}
		} else {
			row, err = transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateLocal}, existing.Epoch,
				workspacestore.StateHandingOff, 1, `,
    handoff_id = $7::uuid, owner_user_id = $8, device_id = $9, device_name = $10, token_id = $11,
    history_mode = $12, has_upstream = $13, excluded = $14::jsonb, stats = $15::jsonb, note = $16,
    expires_at = clock_timestamp() + make_interval(secs => $17),
    base_sha = NULL, upstream_sha = NULL, handoff_sha = NULL, cloud_sha = NULL,
    reclaim_pending = FALSE, reclaim_requested_at = NULL, delete_after = NULL, deleted_at = NULL`,
				handoffID, record.ActorUserID, record.DeviceID, deviceName, record.TokenID, record.HistoryMode,
				record.HasUpstream, string(excluded), string(record.Stats), record.Note, record.TTL.Seconds())
			if err != nil {
				return err
			}
		}
		// A new handoff starts with no refs; earlier segments are superseded
		// (their objects go after the 24-hour grace).
		if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.workspace_refs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
			thread.ProjectID, thread.ConversationUUID); err != nil {
			return fmt.Errorf("workspace handoff: reset refs: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.workspace_segments SET superseded_at = clock_timestamp()
WHERE conversation_uuid = $2::uuid AND project_id = $1 AND superseded_at IS NULL`,
			thread.ProjectID, thread.ConversationUUID); err != nil {
			return fmt.Errorf("workspace handoff: supersede segments: %w", err)
		}
		result.Handoff = row.Handoff
		return nil
	})
	if err != nil {
		return workspacehandoff.BeginResult{}, err
	}
	return result, nil
}

// localWorkConversationSource is conversations.LocalWorkSource (the package
// is not imported here to keep the repository free of the HTTP layer).
const localWorkConversationSource = "local_work"

// nativeDeviceName answers the native device session's name for the token
// family, or "" when it is not a native token or the table is absent.
func nativeDeviceName(ctx context.Context, tx pgx.Tx, tokenID string, userID int64) string {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('elitea_auth.native_sessions') IS NOT NULL`).Scan(&present); err != nil || !present {
		return ""
	}
	token, err := strconv.ParseInt(tokenID, 10, 32)
	if err != nil {
		return ""
	}
	var name string
	if err := tx.QueryRow(ctx, `
SELECT device_name FROM elitea_auth.native_sessions WHERE token_id = $1 AND user_id = $2 LIMIT 1`,
		token, userID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// Complete moves HANDING_OFF to CLOUD_IDLE after checking that the store
// holds the snapshot ref (the refs, not the bytes: the publish checked those).
func (r *WorkspaceHandoffsRepository) Complete(ctx context.Context, record workspacehandoff.CompleteRecord) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var conversation string
		err := tx.QueryRow(ctx, `
SELECT conversation_uuid::text FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND handoff_id = $2::uuid`,
			record.ProjectID, record.HandoffID).Scan(&conversation)
		if errors.Is(err, pgx.ErrNoRows) {
			return workspacehandoff.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("workspace handoff: resolve handoff: %w", err)
		}
		thread := workspacehandoff.Thread{ProjectID: record.ProjectID, ConversationUUID: conversation}
		current, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if !found || current.OwnerUserID != record.ActorUserID || current.HandoffID != record.HandoffID {
			return workspacehandoff.ErrNotFound
		}
		if current.State != workspacestore.StateHandingOff || current.Epoch != record.Epoch ||
			(current.ExpiresAt != nil && !current.ExpiresAt.After(time.Now())) {
			return workspacehandoff.ErrStaleEpoch
		}
		refs := map[string]string{}
		rows, err := tx.Query(ctx, `
SELECT name, sha FROM elitea_runtime.workspace_refs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
			thread.ProjectID, thread.ConversationUUID)
		if err != nil {
			return fmt.Errorf("workspace handoff: read refs: %w", err)
		}
		for rows.Next() {
			var name, sha string
			if err := rows.Scan(&name, &sha); err != nil {
				rows.Close()
				return err
			}
			refs[name] = sha
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if refs[record.HandoffRef] != record.SHA || refs[workspacestore.RefBase] == "" ||
			(current.HasUpstream && refs[workspacestore.RefUpstream] == "") {
			return workspacehandoff.ErrRefsMissing
		}
		row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateHandingOff}, record.Epoch,
			workspacestore.StateCloudIdle, 1, `, expires_at = NULL, cloud_sha = NULL`)
		if err != nil {
			return err
		}
		text := workspacehandoff.CompleteMessage(current.Stats, current.note)
		if err := appendHandoffEvent(ctx, tx, thread, record.Message.Event, text, row.Epoch); err != nil {
			return err
		}
		out = row.Handoff
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) Reclaim(
	ctx context.Context, thread workspacehandoff.Thread, actorUserID, epoch int64, deviceID string,
) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if !found || current.OwnerUserID != actorUserID {
			return workspacehandoff.ErrNotFound
		}
		switch {
		case current.State == workspacestore.StateCloudIdle && current.Epoch == epoch:
			row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateCloudIdle}, epoch,
				workspacestore.StateReclaiming, 1, `, device_id = CASE WHEN $7 = '' THEN device_id ELSE $7 END`, deviceID)
			if err != nil {
				return err
			}
			out = row.Handoff
		case current.State == workspacestore.StateCloudRunning && current.Epoch == epoch:
			row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateCloudRunning}, epoch,
				workspacestore.StateCloudRunning, 0, `, reclaim_pending = TRUE,
    reclaim_requested_at = COALESCE(reclaim_requested_at, clock_timestamp()),
    device_id = CASE WHEN $7 = '' THEN device_id ELSE $7 END`, deviceID)
			if err != nil {
				return err
			}
			out = row.Handoff
		case current.State == workspacestore.StateReclaiming && current.Epoch == epoch+1:
			out = current.Handoff // a retried take back
		default:
			return workspacehandoff.ErrStaleEpoch
		}
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) Release(ctx context.Context, record workspacehandoff.ReleaseRecord) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, record.Thread)
		if err != nil {
			return err
		}
		if !found || current.OwnerUserID != record.ActorUserID {
			return workspacehandoff.ErrNotFound
		}
		if current.State == workspacestore.StateLocal && current.Epoch == record.Epoch+1 && current.DeletedAt == nil {
			out = current.Handoff // a retried release
			return nil
		}
		if current.State != workspacestore.StateReclaiming || current.Epoch != record.Epoch {
			return workspacehandoff.ErrStaleEpoch
		}
		row, err := transition(ctx, tx, record.Thread, []workspacestore.State{workspacestore.StateReclaiming}, record.Epoch,
			workspacestore.StateLocal, 1, `, delete_after = clock_timestamp() + make_interval(secs => $7)`,
			record.Grace.Seconds())
		if err != nil {
			return err
		}
		if err := appendHandoffEvent(ctx, tx, record.Thread, record.Message.Event,
			workspacehandoff.ReleaseMessage(current.DeviceName), row.Epoch); err != nil {
			return err
		}
		out = row.Handoff
		return nil
	})
	return out, err
}

var endedStates = []workspacestore.State{
	workspacestore.StateHandingOff, workspacestore.StateCloudIdle,
	workspacestore.StateCloudRunning, workspacestore.StateReclaiming,
}

const eraseSet = `, delete_after = clock_timestamp(), expires_at = NULL, running_execution_id = NULL,
    running_response_message_id = NULL, reclaim_pending = FALSE, reclaim_requested_at = NULL`

func (r *WorkspaceHandoffsRepository) Erase(ctx context.Context, record workspacehandoff.EraseRecord) (workspacehandoff.EraseResult, error) {
	var out workspacehandoff.EraseResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, record.Thread)
		if err != nil {
			return err
		}
		if !found {
			out.Handoff = workspacehandoff.Handoff{State: workspacestore.StateLocal}
			out.Previous = out.Handoff
			return nil
		}
		if current.OwnerUserID != record.ActorUserID && !record.Manager {
			return workspacehandoff.ErrNotFound
		}
		out.Previous = current.Handoff
		if current.State == workspacestore.StateLocal {
			out.Handoff = current.Handoff
			if current.DeletedAt != nil || (current.DeleteAfter != nil && !current.DeleteAfter.After(time.Now())) {
				return nil
			}
			// A released thread within its return grace: erase now.
			row, err := scanHandoff(tx.QueryRow(ctx, `
UPDATE elitea_runtime.workspace_handoffs SET delete_after = clock_timestamp(), updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid
RETURNING `+handoffColumns, record.Thread.ProjectID, record.Thread.ConversationUUID))
			if err != nil {
				return fmt.Errorf("workspace handoff: erase released: %w", err)
			}
			out.Handoff, out.Changed = row.Handoff, true
			return nil
		}
		row, err := transition(ctx, tx, record.Thread, endedStates, current.Epoch, workspacestore.StateLocal, 1, eraseSet)
		if err != nil {
			return err
		}
		if current.State != workspacestore.StateHandingOff {
			if err := appendHandoffEvent(ctx, tx, record.Thread, record.Message.Event, record.Message.Text, row.Epoch); err != nil {
				return err
			}
		}
		out.Handoff, out.Changed = row.Handoff, true
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) StartCloudTurn(
	ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID, responseMessageID string,
) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if !found {
			return workspacehandoff.ErrNotFound
		}
		if current.State == workspacestore.StateCloudRunning && current.Epoch == epoch &&
			current.RunningExecutionID == executionID {
			out = current.Handoff // a retried start of the same turn
			return nil
		}
		if current.State == workspacestore.StateCloudRunning && current.Epoch == epoch {
			return workspacehandoff.ErrInProgress
		}
		row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateCloudIdle}, epoch,
			workspacestore.StateCloudRunning, 0, `, running_execution_id = $7, running_response_message_id = $8::uuid`,
			executionID, responseMessageID)
		if err != nil {
			return err
		}
		out = row.Handoff
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) FinishCloudTurn(
	ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID string, message *workspacehandoff.EventMessage,
) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if !found {
			return workspacehandoff.ErrNotFound
		}
		if current.State != workspacestore.StateCloudRunning || current.Epoch != epoch ||
			current.RunningExecutionID != executionID {
			return workspacehandoff.ErrStaleEpoch
		}
		to, bump := workspacestore.StateCloudIdle, int64(0)
		if current.ReclaimPending {
			to, bump = workspacestore.StateReclaiming, 1
		}
		row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateCloudRunning}, epoch, to, bump,
			`, running_execution_id = NULL, running_response_message_id = NULL, reclaim_pending = FALSE,
    reclaim_requested_at = NULL`)
		if err != nil {
			return err
		}
		if message != nil {
			if err := appendHandoffEvent(ctx, tx, thread, message.Event, message.Text, row.Epoch); err != nil {
				return err
			}
		}
		out = row.Handoff
		return nil
	})
	return out, err
}

var reclaimNotificationNamespace = uuid.MustParse("5b0f2a5e-3d7c-4c55-9f0e-6f2c8a1d4e71")

func (r *WorkspaceHandoffsRepository) NotifyReclaimRequest(ctx context.Context, handoff workspacehandoff.Handoff) error {
	hour := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	id := uuid.NewSHA1(reclaimNotificationNamespace, []byte(fmt.Sprintf("%d:%s:%d:%s",
		handoff.ProjectID, handoff.ConversationUUID, handoff.Epoch, hour))).String()
	meta, _ := json.Marshal(map[string]any{
		"project_id": handoff.ProjectID, "conversation_uuid": handoff.ConversationUUID, "epoch": handoff.Epoch,
	})
	if _, err := r.pool.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, 'workspace_reclaim_requested')
ON CONFLICT (uuid) DO NOTHING`, id, handoff.ProjectID, handoff.OwnerUserID, string(meta)); err != nil {
		return fmt.Errorf("workspace handoff: notify reclaim request: %w", err)
	}
	return nil
}

func (r *WorkspaceHandoffsRepository) due(ctx context.Context, where string, args ...any) ([]workspacehandoff.Handoff, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+handoffColumns+`
FROM elitea_runtime.workspace_handoffs WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace handoff: due rows: %w", err)
	}
	return scanHandoffs(rows)
}

func (r *WorkspaceHandoffsRepository) timed(
	ctx context.Context, transitionOf workspacehandoff.TimedTransition, from []workspacestore.State,
	to workspacestore.State, set string, args ...any,
) (workspacehandoff.Handoff, error) {
	var out workspacehandoff.Handoff
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		row, err := transition(ctx, tx, transitionOf.Thread, from, transitionOf.Epoch, to, 1, set, args...)
		if err != nil {
			return err
		}
		if transitionOf.Message != nil {
			if err := appendHandoffEvent(ctx, tx, transitionOf.Thread, transitionOf.Message.Event,
				transitionOf.Message.Text, row.Epoch); err != nil {
				return err
			}
		}
		out = row.Handoff
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) DueHandingOff(ctx context.Context, now time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'HANDING_OFF' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
}

func (r *WorkspaceHandoffsRepository) ExpireHandingOff(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateHandingOff}, workspacestore.StateLocal, eraseSet)
}

func (r *WorkspaceHandoffsRepository) DueReclaimGrace(ctx context.Context, before time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'CLOUD_RUNNING' AND reclaim_pending AND reclaim_requested_at <= $1
ORDER BY reclaim_requested_at LIMIT $2`, before, limit)
}

func (r *WorkspaceHandoffsRepository) ForceReclaim(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateCloudRunning}, workspacestore.StateReclaiming,
		`, running_execution_id = NULL, running_response_message_id = NULL, reclaim_pending = FALSE,
    reclaim_requested_at = NULL`)
}

func (r *WorkspaceHandoffsRepository) DueAbandonedReclaim(ctx context.Context, before time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'RECLAIMING' AND state_changed_at <= $1 ORDER BY state_changed_at LIMIT $2`, before, limit)
}

func (r *WorkspaceHandoffsRepository) AbandonReclaim(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateReclaiming}, workspacestore.StateCloudIdle, "")
}

func (r *WorkspaceHandoffsRepository) DueIdle(ctx context.Context, before time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'CLOUD_IDLE' AND state_changed_at <= $1 ORDER BY state_changed_at LIMIT $2`, before, limit)
}

func (r *WorkspaceHandoffsRepository) IdleErase(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateCloudIdle}, workspacestore.StateLocal, eraseSet)
}

func (r *WorkspaceHandoffsRepository) OrphanErase(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	from := append([]workspacestore.State{workspacestore.StateLocal}, endedStates...)
	return r.timed(ctx, t, from, workspacestore.StateLocal, eraseSet)
}

// OrphanedThreads walks the rows not yet erased, in key order from `after`,
// and answers those whose project schema or conversation no longer exists.
func (r *WorkspaceHandoffsRepository) OrphanedThreads(
	ctx context.Context, after workspacehandoff.Thread, limit int,
) ([]workspacehandoff.Handoff, workspacehandoff.Thread, error) {
	afterUUID := after.ConversationUUID
	if afterUUID == "" {
		afterUUID = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := r.due(ctx, `deleted_at IS NULL AND (project_id, conversation_uuid) > ($1, $2::uuid)
ORDER BY project_id, conversation_uuid LIMIT $3`, after.ProjectID, afterUUID, limit)
	if err != nil {
		return nil, workspacehandoff.Thread{}, err
	}
	next := workspacehandoff.Thread{}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = workspacehandoff.Thread{ProjectID: last.ProjectID, ConversationUUID: last.ConversationUUID}
	}
	orphans := []workspacehandoff.Handoff{}
	for _, row := range rows {
		schema, err := tenantSchema(strconv.FormatInt(row.ProjectID, 10))
		if err != nil {
			continue
		}
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".chat_conversations").Scan(&exists); err != nil {
			return nil, workspacehandoff.Thread{}, fmt.Errorf("workspace handoff: tenant schema: %w", err)
		}
		if exists {
			if err := r.pool.QueryRow(ctx, fmt.Sprintf(
				`SELECT EXISTS (SELECT 1 FROM %s.chat_conversations WHERE uuid = $1::uuid)`, schema),
				row.ConversationUUID).Scan(&exists); err != nil {
				return nil, workspacehandoff.Thread{}, fmt.Errorf("workspace handoff: conversation: %w", err)
			}
		}
		if !exists {
			orphans = append(orphans, row)
		}
	}
	return orphans, next, nil
}

// ClaimErasures leases due erasures for a minute (updated_at is the lease), so
// two replicas do not list and delete the same prefix at once.
func (r *WorkspaceHandoffsRepository) ClaimErasures(ctx context.Context, now time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
    SELECT project_id, conversation_uuid FROM elitea_runtime.workspace_handoffs
    WHERE state = 'LOCAL' AND deleted_at IS NULL AND delete_after <= $1
      AND updated_at < $1 - interval '1 minute'
    ORDER BY delete_after
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE elitea_runtime.workspace_handoffs AS h SET updated_at = $1
FROM claimed
WHERE h.project_id = claimed.project_id AND h.conversation_uuid = claimed.conversation_uuid
RETURNING `+qualifiedHandoffColumns("h"), now, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace handoff: claim erasures: %w", err)
	}
	return scanHandoffs(rows)
}

func (r *WorkspaceHandoffsRepository) MarkErased(ctx context.Context, thread workspacehandoff.Thread, epoch int64) error {
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.workspace_handoffs SET deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND state = 'LOCAL' AND epoch = $3
  AND deleted_at IS NULL AND delete_after <= clock_timestamp()`, thread.ProjectID, thread.ConversationUUID, epoch); err != nil {
		return fmt.Errorf("workspace handoff: mark erased: %w", err)
	}
	return nil
}

func qualifiedHandoffColumns(alias string) string {
	return alias + `.project_id, ` + alias + `.conversation_uuid::text, ` + alias + `.handoff_id::text, ` + alias + `.state, ` +
		alias + `.epoch, ` + alias + `.reclaim_pending, ` + alias + `.reclaim_requested_at, ` + alias + `.owner_user_id, ` +
		alias + `.device_id, ` + alias + `.device_name, ` + alias + `.history_mode, ` + alias + `.has_upstream, ` +
		alias + `.base_sha, ` + alias + `.upstream_sha, ` + alias + `.handoff_sha, ` + alias + `.cloud_sha, ` +
		alias + `.stats, ` + alias + `.excluded, ` + alias + `.last_seq, COALESCE(` + alias + `.running_execution_id, ''), ` +
		`COALESCE(` + alias + `.running_response_message_id::text, ''), ` + alias + `.expires_at, ` +
		alias + `.state_changed_at, ` + alias + `.updated_at, ` + alias + `.delete_after, ` + alias + `.deleted_at, ` + alias + `.note`
}

// handoffEventParticipantKind marks the tenant schema's system participant
// that authors handoff event messages.
const handoffEventParticipantKind = "workspace_handoff"

// appendHandoffEvent writes one system-authored message into the thread's
// conversation, in the caller's transaction. A conversation that is gone
// takes no message.
func appendHandoffEvent(ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread, event, text string, epoch int64) error {
	if text == "" {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(thread.ProjectID, 10))
	if err != nil {
		return workspacehandoff.ErrInvalid
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".chat_message_group").Scan(&present); err != nil {
		return fmt.Errorf("workspace handoff: tenant schema: %w", err)
	}
	if !present {
		return nil
	}
	var conversationID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema),
		thread.ConversationUUID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace handoff: event conversation: %w", err)
	}
	var participantID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = 'system' AND entity_meta ->> 'kind' = $1
ORDER BY id LIMIT 1`, schema), handoffEventParticipantKind).Scan(&participantID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'system', jsonb_build_object('kind', $1::text), '{}'::json)
RETURNING id`, schema), handoffEventParticipantKind).Scan(&participantID)
	}
	if err != nil {
		return fmt.Errorf("workspace handoff: event participant: %w", err)
	}
	meta, _ := json.Marshal(map[string]any{"workspace_event": event, "workspace_epoch": epoch})
	var groupID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
VALUES (gen_random_uuid(), $1, $2, $3::jsonb, FALSE, GREATEST(
    clock_timestamp(),
    (SELECT max(created_at) + interval '1 millisecond'
     FROM %[1]s.chat_message_group WHERE conversation_id = $2)))
RETURNING id`, schema), participantID, conversationID, string(meta)).Scan(&groupID); err != nil {
		return fmt.Errorf("workspace handoff: event message: %w", err)
	}
	return insertLocalTurnText(ctx, tx, schema, groupID, text)
}

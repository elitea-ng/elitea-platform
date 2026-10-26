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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
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
       state_changed_at, updated_at, delete_after, deleted_at, note,
       included_excluded, env_names, COALESCE(env_secret_name, ''), released, rest_pending,
       COALESCE(delete_after > clock_timestamp(), FALSE)`

type handoffRow struct {
	workspacehandoff.Handoff
	note string
}

func scanHandoff(row pgx.Row) (handoffRow, error) {
	var (
		out                   handoffRow
		state                 string
		stats                 []byte
		excluded              []byte
		includes, environment []byte
	)
	err := row.Scan(&out.ProjectID, &out.ConversationUUID, &out.HandoffID, &state, &out.Epoch, &out.ReclaimPending,
		&out.ReclaimRequestedAt, &out.OwnerUserID, &out.DeviceID, &out.DeviceName, &out.HistoryMode, &out.HasUpstream,
		&out.BaseSHA, &out.UpstreamSHA, &out.HandoffSHA, &out.CloudSHA, &stats, &excluded, &out.LastSeq,
		&out.RunningExecutionID, &out.RunningResponseID, &out.ExpiresAt,
		&out.StateChangedAt, &out.UpdatedAt, &out.DeleteAfter, &out.DeletedAt, &out.note,
		&includes, &environment, &out.EnvSecretName, &out.Released, &out.RestPending, &out.ReturnGraceOpen)
	if err != nil {
		return handoffRow{}, err
	}
	out.State = workspacestore.State(state)
	out.Stats = json.RawMessage(stats)
	_ = json.Unmarshal(excluded, &out.Excluded)
	if out.Excluded == nil {
		out.Excluded = []string{}
	}
	_ = json.Unmarshal(includes, &out.IncludedExcluded)
	if out.IncludedExcluded == nil {
		out.IncludedExcluded = []string{}
	}
	_ = json.Unmarshal(environment, &out.EnvNames)
	if out.EnvNames == nil {
		out.EnvNames = []string{}
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
	// Every transition clears the release marker except a release, which
	// sets it in its own `set`.
	released := ", released = FALSE"
	if strings.Contains(set, "released = TRUE") {
		released = ""
	}
	// Likewise the rest marker: only a release or a rest take back assigns it.
	if !strings.Contains(set, "rest_pending =") {
		released += ", rest_pending = FALSE"
	}
	query := `UPDATE elitea_runtime.workspace_handoffs
SET ` + stateChanged + `, state = $3, epoch = epoch + $4, updated_at = clock_timestamp()` + released + set + `
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
	if len(from) != 1 || from[0] != to {
		if err := stampHandoffConversation(ctx, tx, thread); err != nil {
			return handoffRow{}, err
		}
	}
	return row, nil
}

// stampHandoffConversation moves the conversation's sync_at, so the list
// delta re-delivers the row with its new `workspace_executor` (the tenant's
// BEFORE UPDATE trigger, tenant/0144, writes the stamp). A schema without the
// column, or a conversation that is gone, is left alone.
func stampHandoffConversation(ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread) error {
	schema, err := tenantSchema(strconv.FormatInt(thread.ProjectID, 10))
	if err != nil {
		return nil
	}
	var present bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_attribute
               WHERE attrelid = to_regclass($1) AND attname = 'sync_at' AND NOT attisdropped)`,
		schema+".chat_conversations").Scan(&present); err != nil {
		return fmt.Errorf("workspace handoff: conversation sync column: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_conversations SET sync_at = clock_timestamp() WHERE uuid = $1::uuid`, schema),
		thread.ConversationUUID); err != nil {
		return fmt.Errorf("workspace handoff: stamp conversation: %w", err)
	}
	return nil
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
			}
			if existing.DeletedAt == nil && existing.DeleteAfter != nil {
				// The previous cloud copy is being erased: the erasure owns the
				// thread prefix until its listing is empty. delete_after is
				// stamped with the database clock, so it is compared there;
				// against this process's clock a skew either way misreads it.
				var erasing bool
				if err := tx.QueryRow(ctx, `SELECT $1::timestamptz <= clock_timestamp()`, *existing.DeleteAfter).Scan(&erasing); err != nil {
					return fmt.Errorf("workspace handoff: read erasure: %w", err)
				}
				if erasing {
					return workspacehandoff.ErrInProgress
				}
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
		includes, _ := json.Marshal(record.IncludedExcluded)
		// The environment: kept from the previous send, replaced, or cleared.
		envNames, _ := json.Marshal(record.Env.Names)
		var envSecret *string
		if record.Env.SecretName != "" {
			name := record.Env.SecretName
			envSecret = &name
		}
		if found && record.Env.Keep {
			envNames, _ = json.Marshal(existing.EnvNames)
			if existing.EnvSecretName != "" {
				name := existing.EnvSecretName
				envSecret = &name
			}
		}
		if found && !record.Env.Keep && existing.EnvSecretName != "" {
			result.PreviousEnvSecret = existing.EnvSecretName
		}
		handoffID := uuid.NewString()
		var row handoffRow
		if !found {
			row, err = scanHandoff(tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (
    project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id, device_id, device_name, token_id,
    history_mode, has_upstream, excluded, stats, note, expires_at, included_excluded, env_names, env_secret_name
) VALUES ($1, $2::uuid, $3::uuid, 'HANDING_OFF', 1, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12,
          clock_timestamp() + make_interval(secs => $13), $14::jsonb, $15::jsonb, $16)
RETURNING `+handoffColumns,
				thread.ProjectID, thread.ConversationUUID, handoffID, record.ActorUserID, record.DeviceID, deviceName,
				record.TokenID, record.HistoryMode, record.HasUpstream, string(excluded), string(record.Stats),
				record.Note, record.TTL.Seconds(), string(includes), string(envNames), envSecret))
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
    reclaim_pending = FALSE, reclaim_requested_at = NULL, delete_after = NULL, deleted_at = NULL,
    included_excluded = $18::jsonb, env_names = $19::jsonb, env_secret_name = $20`,
				handoffID, record.ActorUserID, record.DeviceID, deviceName, record.TokenID, record.HistoryMode,
				record.HasUpstream, string(excluded), string(record.Stats), record.Note, record.TTL.Seconds(),
				string(includes), string(envNames), envSecret)
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
) (workspacehandoff.TransitionResult, error) {
	var out workspacehandoff.TransitionResult
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
			out = workspacehandoff.TransitionResult{Handoff: row.Handoff, Changed: true}
		case current.State == workspacestore.StateCloudRunning && current.Epoch == epoch:
			row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateCloudRunning}, epoch,
				workspacestore.StateCloudRunning, 0, `, reclaim_pending = TRUE,
    reclaim_requested_at = COALESCE(reclaim_requested_at, clock_timestamp()),
    device_id = CASE WHEN $7 = '' THEN device_id ELSE $7 END`, deviceID)
			if err != nil {
				return err
			}
			// A repeated take back of a running turn finds the flag set.
			out = workspacehandoff.TransitionResult{Handoff: row.Handoff, Changed: !current.ReclaimPending}
		case current.State == workspacestore.StateReclaiming && current.Epoch == epoch+1:
			out = workspacehandoff.TransitionResult{Handoff: current.Handoff} // a retried take back
		default:
			return workspacehandoff.ErrStaleEpoch
		}
		return nil
	})
	return out, err
}

// ReclaimRest is the rest take back: a released LOCAL row whose release left
// turns not taken goes back to RECLAIMING (epoch + 1) within its return
// grace, keeping rest_pending and delete_after. It takes the thread lock
// exclusively, as a send does, so no local turn starts while it checks that
// none is uncommitted.
func (r *WorkspaceHandoffsRepository) ReclaimRest(
	ctx context.Context, thread workspacehandoff.Thread, actorUserID, epoch int64, deviceID string,
) (workspacehandoff.TransitionResult, error) {
	var out workspacehandoff.TransitionResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceThread(ctx, tx, thread.ProjectID, thread.ConversationUUID, false); err != nil {
			return err
		}
		current, found, err := lockHandoff(ctx, tx, thread)
		if err != nil {
			return err
		}
		if !found || current.OwnerUserID != actorUserID {
			return workspacehandoff.ErrNotFound
		}
		if current.State == workspacestore.StateReclaiming && current.Epoch == epoch+1 && current.RestPending {
			out = workspacehandoff.TransitionResult{Handoff: current.Handoff} // a retried rest take back
			return nil
		}
		if current.State != workspacestore.StateLocal || current.Epoch != epoch {
			return workspacehandoff.ErrStaleEpoch
		}
		// delete_after is stamped with the database clock, so the grace is
		// compared there.
		inGrace := false
		if current.DeletedAt == nil && current.DeleteAfter != nil {
			if err := tx.QueryRow(ctx, `SELECT $1::timestamptz > clock_timestamp()`, *current.DeleteAfter).Scan(&inGrace); err != nil {
				return fmt.Errorf("workspace handoff: read return grace: %w", err)
			}
		}
		if !inGrace {
			return workspacehandoff.ErrErased
		}
		if !current.Released || !current.RestPending {
			return workspacehandoff.ErrStaleEpoch
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
		row, err := transition(ctx, tx, thread, []workspacestore.State{workspacestore.StateLocal}, epoch,
			workspacestore.StateReclaiming, 1, `, rest_pending = TRUE,
    device_id = CASE WHEN $7 = '' THEN device_id ELSE $7 END`, deviceID)
		if err != nil {
			return err
		}
		out = workspacehandoff.TransitionResult{Handoff: row.Handoff, Changed: true}
		return nil
	})
	return out, err
}

func (r *WorkspaceHandoffsRepository) Release(ctx context.Context, record workspacehandoff.ReleaseRecord) (workspacehandoff.TransitionResult, error) {
	var out workspacehandoff.TransitionResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, found, err := lockHandoff(ctx, tx, record.Thread)
		if err != nil {
			return err
		}
		if !found || current.OwnerUserID != record.ActorUserID {
			return workspacehandoff.ErrNotFound
		}
		if current.State == workspacestore.StateLocal && current.Epoch == record.Epoch+1 {
			// LOCAL one epoch on: a retried release only if a release put it
			// there. An erase (or expiry, or offboarding) that won the race
			// is reported as such, never as a successful release.
			var released bool
			if err := tx.QueryRow(ctx, `
SELECT released FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
				record.Thread.ProjectID, record.Thread.ConversationUUID).Scan(&released); err != nil {
				return fmt.Errorf("workspace handoff: read release marker: %w", err)
			}
			if !released {
				return workspacehandoff.ErrErased
			}
			if current.RestPending && !record.RestPending {
				// The rest take back this release ends was abandoned by the
				// sweeper (LOCAL, the rest still pending): not this release.
				return workspacehandoff.ErrStaleEpoch
			}
			out = workspacehandoff.TransitionResult{Handoff: current.Handoff} // a retried release
			return nil
		}
		if current.State == workspacestore.StateLocal {
			var released bool
			if err := tx.QueryRow(ctx, `
SELECT released FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
				record.Thread.ProjectID, record.Thread.ConversationUUID).Scan(&released); err != nil {
				return fmt.Errorf("workspace handoff: read release marker: %w", err)
			}
			if !released && current.Epoch > record.Epoch {
				return workspacehandoff.ErrErased
			}
		}
		if current.State != workspacestore.StateReclaiming || current.Epoch != record.Epoch {
			return workspacehandoff.ErrStaleEpoch
		}
		// The release of a rest take back keeps the first release's grace:
		// the remaining turns never extend how long the cloud copy lives.
		row, err := transition(ctx, tx, record.Thread, []workspacestore.State{workspacestore.StateReclaiming}, record.Epoch,
			workspacestore.StateLocal, 1, `, released = TRUE, rest_pending = $8,
    delete_after = CASE WHEN rest_pending AND delete_after IS NOT NULL THEN delete_after
                        ELSE clock_timestamp() + make_interval(secs => $7) END`,
			record.Grace.Seconds(), record.RestPending)
		if err != nil {
			return err
		}
		if err := appendHandoffEvent(ctx, tx, record.Thread, record.Message.Event,
			workspacehandoff.ReleaseMessage(current.DeviceName), row.Epoch); err != nil {
			return err
		}
		out = workspacehandoff.TransitionResult{Handoff: row.Handoff, Changed: true}
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
UPDATE elitea_runtime.workspace_handoffs
SET delete_after = clock_timestamp(), updated_at = clock_timestamp(), rest_pending = FALSE
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
) (workspacehandoff.TransitionResult, error) {
	var out workspacehandoff.TransitionResult
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
			// A retried start of the same turn (a repeated redemption). The
			// rewind is answered again only while the ref still holds the
			// commit to rewind from, so a rewind is never applied twice.
			rewind, err := startCloudTurnMarks(ctx, tx, thread.ProjectID, responseMessageID, current.CloudSHA, current.HandoffSHA)
			if err != nil {
				return err
			}
			out = workspacehandoff.TransitionResult{Handoff: current.Handoff}
			out.Handoff.RewindTo = rewind
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
		cloud := ""
		if row.CloudSHA != nil {
			cloud = *row.CloudSHA
		}
		// The turn's answer carries the markers from its start; a publish
		// moves workspace_commit to the turn's own commit.
		if err := StampCloudTurnAnswer(ctx, tx, thread.ProjectID, responseMessageID, epoch, cloud); err != nil {
			return err
		}
		rewind, err := startCloudTurnMarks(ctx, tx, thread.ProjectID, responseMessageID, row.CloudSHA, row.HandoffSHA)
		if err != nil {
			return err
		}
		out = workspacehandoff.TransitionResult{Handoff: row.Handoff, Changed: true}
		out.Handoff.RewindTo = rewind
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
	// conversation_id carries the conversation UUID (the web toast matches
	// it); conversation_uuid repeats it under its own name.
	meta, _ := json.Marshal(map[string]any{
		"project_id": handoff.ProjectID, "conversation_id": handoff.ConversationUUID,
		"conversation_uuid": handoff.ConversationUUID, "epoch": handoff.Epoch,
	})
	if _, err := r.pool.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, 'workspace_reclaim_requested',
        now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
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

// DeadCloudTurns answers CLOUD_RUNNING threads whose worker died: the
// execution is not terminal yet (a terminal one is FinishedCloudTurns'), and
// its newest claim was never released and its lease ran out more than grace
// ago. A released claim is a worker handing the execution back (retry,
// requeue): the execution waits for its next claim and is not reaped.
func (r *WorkspaceHandoffsRepository) DeadCloudTurns(ctx context.Context, grace time.Duration, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'CLOUD_RUNNING'
  AND state_changed_at < clock_timestamp() - make_interval(secs => $1)
  AND EXISTS (SELECT 1 FROM elitea_runtime.execution_jobs AS j
              WHERE j.execution_id = running_execution_id
                AND j.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'))
  AND EXISTS (SELECT 1 FROM (SELECT c.released_at, c.lease_expires_at
                             FROM elitea_runtime.execution_claims AS c
                             WHERE c.execution_id = running_execution_id
                             ORDER BY c.claimed_at DESC LIMIT 1) AS latest
              WHERE latest.released_at IS NULL
                AND latest.lease_expires_at < clock_timestamp() - make_interval(secs => $1))
ORDER BY state_changed_at LIMIT $2`, grace.Seconds(), limit)
}

func (r *WorkspaceHandoffsRepository) DueAbandonedReclaim(ctx context.Context, before time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'RECLAIMING' AND NOT rest_pending AND state_changed_at <= $1
ORDER BY state_changed_at LIMIT $2`, before, limit)
}

// AbandonReclaim moves only what DueAbandonedReclaim answered: the epoch
// compare-and-set keeps a rest take back (never due there) where it is.
func (r *WorkspaceHandoffsRepository) AbandonReclaim(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateReclaiming}, workspacestore.StateCloudIdle, "")
}

func (r *WorkspaceHandoffsRepository) DueRestExpired(ctx context.Context, now time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'RECLAIMING' AND rest_pending AND delete_after <= $1
ORDER BY delete_after LIMIT $2`, now, limit)
}

func (r *WorkspaceHandoffsRepository) DueAbandonedRestReclaim(ctx context.Context, before time.Time, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'RECLAIMING' AND rest_pending AND state_changed_at <= $1
  AND deleted_at IS NULL AND delete_after > clock_timestamp()
ORDER BY state_changed_at LIMIT $2`, before, limit)
}

// AbandonRestReclaim returns a rest take back nobody released to LOCAL,
// released, with the rest still pending and the first release's
// delete_after kept (a transition never moves it).
func (r *WorkspaceHandoffsRepository) AbandonRestReclaim(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateReclaiming}, workspacestore.StateLocal,
		`, released = TRUE, rest_pending = TRUE`)
}

// ExpireRestReclaim returns a rest take back whose return grace ran out to
// LOCAL, released, keeping delete_after, so the erasure removes the copy.
func (r *WorkspaceHandoffsRepository) ExpireRestReclaim(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateReclaiming}, workspacestore.StateLocal,
		`, released = TRUE, rest_pending = FALSE`)
}

func (r *WorkspaceHandoffsRepository) DueIdle(
	ctx context.Context, after workspacehandoff.IdleCursor, limit int,
) ([]workspacehandoff.Handoff, error) {
	afterUUID := after.ConversationUUID
	if afterUUID == "" {
		afterUUID = "00000000-0000-0000-0000-000000000000"
	}
	return r.due(ctx, `state = 'CLOUD_IDLE'
  AND (state_changed_at, project_id, conversation_uuid) > ($1::timestamptz, $2::integer, $3::uuid)
ORDER BY state_changed_at, project_id, conversation_uuid LIMIT $4`, after.StateChangedAt, after.ProjectID, afterUUID, limit)
}

var idleNotificationNamespace = uuid.MustParse("0c6f3e4b-8a51-4f0d-9d2e-71a4c5b9e6f3")

// NotifyIdleExpiring writes the owner's warning before the idle-retention
// erasure. The notification uuid is derived from the thread, the epoch and
// the start of the idle period, so each idle period warns once.
func (r *WorkspaceHandoffsRepository) NotifyIdleExpiring(
	ctx context.Context, handoff workspacehandoff.Handoff, deleteAt time.Time,
) (bool, error) {
	id := uuid.NewSHA1(idleNotificationNamespace, []byte(fmt.Sprintf("%d:%s:%d:%d",
		handoff.ProjectID, handoff.ConversationUUID, handoff.Epoch, handoff.StateChangedAt.UnixMicro()))).String()
	meta, _ := json.Marshal(map[string]any{
		"project_id": handoff.ProjectID, "conversation_id": handoff.ConversationUUID,
		"conversation_uuid": handoff.ConversationUUID, "epoch": handoff.Epoch,
		"delete_after": deleteAt.UTC().Format(time.RFC3339),
		// The notification list renders meta.message as is.
		"message": "The cloud copy of a Local work thread is erased on " + deleteAt.UTC().Format("2 January 2006") +
			" because nobody used it. Send a prompt to the thread or take it back to keep it.",
	})
	tag, err := r.pool.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, 'workspace_idle_expiring',
        now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`, id, handoff.ProjectID, handoff.OwnerUserID, string(meta))
	if err != nil {
		return false, fmt.Errorf("workspace handoff: notify idle expiry: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *WorkspaceHandoffsRepository) IdleErase(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	return r.timed(ctx, t, []workspacestore.State{workspacestore.StateCloudIdle}, workspacestore.StateLocal, eraseSet)
}

func (r *WorkspaceHandoffsRepository) OrphanErase(ctx context.Context, t workspacehandoff.TimedTransition) (workspacehandoff.Handoff, error) {
	from := append([]workspacestore.State{workspacestore.StateLocal}, endedStates...)
	return r.timed(ctx, t, from, workspacestore.StateLocal, eraseSet)
}

// OrphanedThreads walks the rows not yet erased, in key order from `after`,
// and answers those whose project schema or conversation no longer exists,
// and those whose owner is offboarded: deleted, or suspended (an
// administrator's suspension and a SCIM deactivation both set
// auth_core__user.suspended). ADR-0034 decision 12 erases a thread on user
// offboarding; finding it here covers every offboarding path (the admin
// routes, SCIM, a direct change) with the same erasure the sweeper runs for
// a deleted conversation, which also stops a running cloud turn.
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
	// The users table belongs to the auth migrations, not the shared ones
	// (the shared migrations guard it with to_regclass the same way); a
	// database without it has no owner to offboard.
	var usersTable bool
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__user') IS NOT NULL`).Scan(&usersTable); err != nil {
		return nil, workspacehandoff.Thread{}, fmt.Errorf("workspace handoff: users table: %w", err)
	}
	orphans := []workspacehandoff.Handoff{}
	for _, row := range rows {
		if usersTable {
			var ownerActive bool
			if err := r.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1 AND suspended = false)`,
				row.OwnerUserID).Scan(&ownerActive); err != nil {
				return nil, workspacehandoff.Thread{}, fmt.Errorf("workspace handoff: owner: %w", err)
			}
			if !ownerActive {
				orphans = append(orphans, row)
				continue
			}
		}
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
UPDATE elitea_runtime.workspace_handoffs
SET deleted_at = clock_timestamp(), updated_at = clock_timestamp(), env_secret_name = NULL, env_names = '[]'::jsonb
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
		alias + `.state_changed_at, ` + alias + `.updated_at, ` + alias + `.delete_after, ` + alias + `.deleted_at, ` + alias + `.note, ` +
		alias + `.included_excluded, ` + alias + `.env_names, COALESCE(` + alias + `.env_secret_name, ''), ` +
		alias + `.released, ` + alias + `.rest_pending, COALESCE(` + alias + `.delete_after > clock_timestamp(), FALSE)`
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
    clock_timestamp() AT TIME ZONE 'UTC',
    (SELECT max(created_at) + interval '1 millisecond'
     FROM %[1]s.chat_message_group WHERE conversation_id = $2)))
RETURNING id`, schema), participantID, conversationID, string(meta)).Scan(&groupID); err != nil {
		return fmt.Errorf("workspace handoff: event message: %w", err)
	}
	return insertLocalTurnText(ctx, tx, schema, groupID, text)
}

// ReadByRunningExecution answers the thread a running cloud turn belongs to
// (the executor's claim resolves its execution id to the thread).
func (r *WorkspaceHandoffsRepository) ReadByRunningExecution(ctx context.Context, executionID string) (workspacehandoff.Handoff, bool, error) {
	row, err := scanHandoff(r.pool.QueryRow(ctx, `SELECT `+handoffColumns+`
FROM elitea_runtime.workspace_handoffs WHERE running_execution_id = $1 ORDER BY state_changed_at DESC LIMIT 1`, executionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacehandoff.Handoff{}, false, nil
	}
	if err != nil {
		return workspacehandoff.Handoff{}, false, fmt.Errorf("workspace handoff: read by execution: %w", err)
	}
	return row.Handoff, true, nil
}

// startCloudTurnMarks runs at a cloud turn's claim, in its transaction. It
// answers the commit to rewind refs/elitea/cloud to when the answer is a
// regenerate of the newest cloud turn (MetaRewindFrom/MetaRewindTo, written
// by the regenerate) and the ref still holds the commit to rewind from. It
// stamps MetaWorkspaceBase, the commit the turn starts from, only when the
// answer has none: the rewind target, else the cloud ref, else the handoff
// snapshot. A continuation of a paused turn keeps the base of its first claim.
func startCloudTurnMarks(ctx context.Context, tx pgx.Tx, projectID int64, responseMessageID string, cloud, handoffSHA *string) (*string, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, nil
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".chat_message_group").Scan(&present); err != nil {
		return nil, fmt.Errorf("workspace store: tenant schema: %w", err)
	}
	if !present {
		return nil, nil
	}
	var from, to *string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT meta ->> '%s', meta ->> '%s' FROM %s.chat_message_group WHERE uuid = $1::uuid`,
		workspacehandoff.MetaRewindFrom, workspacehandoff.MetaRewindTo, schema), responseMessageID).Scan(&from, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace store: read the answer's rewind: %w", err)
	}
	var rewind *string
	if from != nil && to != nil && cloud != nil && *from == *cloud &&
		workspacestore.ValidSHA1(*to) && *to != *cloud {
		value := *to
		rewind = &value
	}
	base := rewind
	if base == nil {
		base = cloud
	}
	if base == nil {
		base = handoffSHA
	}
	if base == nil {
		return rewind, nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_message_group SET meta = jsonb_build_object('%s', $2::text) || meta WHERE uuid = $1::uuid`,
		schema, workspacehandoff.MetaWorkspaceBase), responseMessageID, *base); err != nil {
		return nil, fmt.Errorf("workspace store: stamp the turn's base commit: %w", err)
	}
	return rewind, nil
}

// CloudTurnExecution is what a cloud turn's claim needs from the execution:
// its chat answer, its conversation and when it was admitted.
type CloudTurnExecution struct {
	ResponseMessageID string
	ConversationUUID  string
	AdmittedAt        time.Time
}

// CloudTurnExecution reads the agent execution's chat binding. false when the
// execution is not an agent execution of a conversation.
func (r *WorkspaceHandoffsRepository) CloudTurnExecution(ctx context.Context, executionID string, generation uint64) (CloudTurnExecution, bool, error) {
	if generation == 0 || generation > 1<<62 {
		return CloudTurnExecution{}, false, nil
	}
	var out CloudTurnExecution
	err := r.pool.QueryRow(ctx, `
SELECT a.client_message_id, a.client_stream_id, j.admitted_at
FROM elitea_runtime.agent_execution_jobs AS a
JOIN elitea_runtime.execution_jobs AS j ON j.execution_id = a.execution_id AND j.generation = a.generation
WHERE a.execution_id = $1 AND a.generation = $2`, executionID, int64(generation)).
		Scan(&out.ResponseMessageID, &out.ConversationUUID, &out.AdmittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudTurnExecution{}, false, nil
	}
	if err != nil {
		return CloudTurnExecution{}, false, fmt.Errorf("workspace handoff: read the cloud turn's execution: %w", err)
	}
	return out, true, nil
}

// ReclaimPending answers whether executionID is the running cloud turn of a
// thread with a take back pending (the partial running_execution_id index).
func (r *WorkspaceHandoffsRepository) ReclaimPending(ctx context.Context, executionID string) (bool, error) {
	var pending bool
	err := r.pool.QueryRow(ctx, `
SELECT reclaim_pending FROM elitea_runtime.workspace_handoffs
WHERE running_execution_id = $1 AND state = 'CLOUD_RUNNING'`, executionID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workspace handoff: read the take-back signal: %w", err)
	}
	return pending, nil
}

// FinishedCloudTurns answers CLOUD_RUNNING threads whose running execution has
// no generation left that is not terminal (SUCCEEDED, FAILED, CANCELLED or
// QUARANTINED): the turn ended but its finish never ran.
func (r *WorkspaceHandoffsRepository) FinishedCloudTurns(ctx context.Context, limit int) ([]workspacehandoff.Handoff, error) {
	return r.due(ctx, `state = 'CLOUD_RUNNING'
  AND EXISTS (SELECT 1 FROM elitea_runtime.execution_jobs AS j WHERE j.execution_id = running_execution_id)
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.execution_jobs AS j
                  WHERE j.execution_id = running_execution_id
                    AND j.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'))
ORDER BY state_changed_at LIMIT $1`, limit)
}

// cloudRegenerateRewind is what a regenerate of a handed-off thread's answer
// carries to its claim (decision 7). present: the conversation has a handoff
// row, so the answer's base and rewind marks are reset; from/to: the rewind,
// when the answer is the newest cloud turn and it published the commit the
// cloud ref holds now.
type cloudRegenerateRewind struct {
	present  bool
	from, to string
}

// readCloudRegenerateRewind reads, inside the admission transaction and
// before the reset strips them, the answer's cloud markers and the thread's
// cloud ref. A rewind is due only when the answer is a cloud turn
// (executed_by), the thread is CLOUD_IDLE, the answer's workspace_commit is
// the cloud ref now, and it differs from the commit the turn started from
// (workspace_base_commit): the turn published. A turn that failed before it
// published keeps workspace_commit == base and rewinds nothing, so a
// regenerate never undoes an earlier turn's work.
func readCloudRegenerateRewind(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID, responseMessageID string) (cloudRegenerateRewind, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return cloudRegenerateRewind{}, nil
	}
	var (
		executedBy          string
		commit, base, cloud *string
		state               *string
	)
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(g.meta ->> '%s', ''), g.meta ->> '%s', g.meta ->> '%s', h.state, h.cloud_sha
FROM %s.chat_message_group AS g
LEFT JOIN elitea_runtime.workspace_handoffs AS h ON h.project_id = $2 AND h.conversation_uuid = $3::uuid
WHERE g.uuid = $1::uuid`, workspacehandoff.MetaExecutedBy, workspacehandoff.MetaWorkspaceCommit,
		workspacehandoff.MetaWorkspaceBase, schema), responseMessageID, projectID, conversationUUID).
		Scan(&executedBy, &commit, &base, &state, &cloud)
	if errors.Is(err, pgx.ErrNoRows) {
		return cloudRegenerateRewind{}, nil
	}
	if err != nil {
		return cloudRegenerateRewind{}, fmt.Errorf("read the regenerated answer's cloud markers: %w", err)
	}
	if state == nil {
		return cloudRegenerateRewind{}, nil
	}
	out := cloudRegenerateRewind{present: true}
	if executedBy == workspacehandoff.ExecutedByCloud && *state == string(workspacestore.StateCloudIdle) &&
		commit != nil && base != nil && cloud != nil && *commit == *cloud && *base != *commit &&
		workspacestore.ValidSHA1(*commit) && workspacestore.ValidSHA1(*base) {
		out.from, out.to = *commit, *base
	}
	return out, nil
}

// writeCloudRegenerateRewind resets the answer's base and rewind marks (the
// regenerated answer is a new cloud turn) and writes the rewind, if any, for
// the claim to read (startCloudTurnMarks).
func writeCloudRegenerateRewind(ctx context.Context, tx pgx.Tx, projectID int64, responseMessageID string, rewind cloudRegenerateRewind) error {
	if !rewind.present {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil
	}
	marks := map[string]string{}
	if rewind.from != "" {
		marks[workspacehandoff.MetaRewindFrom], marks[workspacehandoff.MetaRewindTo] = rewind.from, rewind.to
	}
	encoded, _ := json.Marshal(marks)
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_message_group SET meta = (meta - '%s' - '%s' - '%s') || $2::jsonb WHERE uuid = $1::uuid`,
		schema, workspacehandoff.MetaWorkspaceBase, workspacehandoff.MetaRewindFrom, workspacehandoff.MetaRewindTo),
		responseMessageID, string(encoded)); err != nil {
		return fmt.Errorf("mark the regenerated answer's rewind: %w", err)
	}
	return nil
}

package toolkits

// The three write paths behind the toolkit "Indexes" tab.
//
// Until #180 all three were one-line stubs in handler.go: `IndexMetaUpdate`
// and `IndexCancel` answered `{"ok":true}`, `IndexMetaDelete` answered 204 —
// none of them opened a database connection. They were dormant while the tab
// rendered an empty <Box>; mounting `IndexesContainer` (#149, PR #179)
// connected a working UI to handlers that reported success and changed
// nothing, so deleting an index appeared to work and the row was still there
// on reload.
//
// The behaviour implemented here is taken from the legacy handlers that
// own this contract today — `legacy/plugins/elitea_core/api/v2/index_meta.py`
// and `.../index_cancel.py` — reduced to the columns this generation's
// `p_<project>.index_meta` table actually has (id, toolkit_id, name, status,
// progress, meta, created_at; see internal/infra/db/migrations/001_initial.sql).
// The legacy runtime keeps the same records as rows in the per-toolkit PgVector
// `langchain_pg_embedding` table keyed by `cmetadata->>'collection'`; the
// column names differ, the semantics below do not.
//
// The client is `apps/elitea-web/src/features/toolkits/indexes/api/indexesApi.ts`.
// What it actually sends, which is not what the path parameter names suggest:
//
//   deleteIndexItem      DELETE .../index_meta/prompt_lib/{p}/{tk}/{indexId}
//                        body {"is_hidden": true} — ignored, see below
//   updateIndexSchedule  PATCH  .../index_meta/prompt_lib/{p}/{tk}/{indexName}
//                        body {timezone, cron?, enabled?, credentials?}
//   stopIndexingItem     DELETE .../index_cancel/prompt_lib/{p}/{tk}/{indexName}/{taskId}
//
// Note the asymmetry the route parameter `{indexMetaID}` hides: DELETE
// addresses a row by its **id**, PATCH addresses a schedule bucket by the
// index **name**. That is not a client bug — legacy keys `indexes_meta` by
// name too (`clean_up_schedule_in_toolkit` pops it by `index_name`), and the
// read side agrees: `getIndexSchedule` reads
// `elitea_tools.meta.indexes_meta[<index name>].schedules[<user id>]`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// defaultScheduleUserID is the legacy runtime's "this schedule applies to every user"
// bucket (`UpdateIndexingSchedule.user_id` defaults to -1, and the web client
// never sends a user_id at all). `resolveScheduleData` in IndexActions.tsx
// falls back to exactly this key when the caller has no per-user entry.
const defaultScheduleUserID = -1

// cancellableIndexStatuses is the set of `index_meta.status` values that
// describe a run that has not finished. Anything else is terminal, and
// cancelling it is a client mistake rather than a no-op — see IndexCancel.
var cancellableIndexStatuses = map[string]bool{
	"created":     true,
	"pending":     true,
	"queued":      true,
	"in_progress": true,
	"running":     true,
}

// tenantSchemaName resolves the tenant schema for a project id and returns it
// as a QUOTED PostgreSQL identifier, ready to interpolate with %s.
//
// Every read and write in this package goes through the same quoting now. A
// project id that is not a plain decimal number is refused before it reaches a
// statement, and the identifier is quoted with SQL rules rather than with %q,
// which quotes with Go rules and lets an embedded quote end the identifier
// (issue #543).
func tenantSchemaName(projectID string) (string, error) {
	return tenantschema.Quote(projectID)
}

func toolkitRowID(toolkitID string) (int64, error) {
	id, err := strconv.ParseInt(toolkitID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a toolkit id", toolkitID)
	}
	return id, nil
}

// writeIndexInternalError answers a fixed, safe message and logs the cause.
//
// DEFECT: these handlers put `err.Error()` straight into the 500 body. The
// value is an unwrapped pgx error. When PostgreSQL is unreachable,
// `*pgconn.ConnectError` prints
// `failed to connect to `user=<db user> database=<db name>`: <host>:<port> ...`,
// so any authenticated caller read the internal database user, database name,
// host and port. A `*pgconn.PgError` prints `ERROR: <message> (SQLSTATE nnnnn)`
// and can carry constraint names and table names that the route does not.
// AGENTS.md forbids a raw `err.Error()` across a trust boundary.
//
// The envelope keeps its shape: the client reads `ok`.
func writeIndexInternalError(w http.ResponseWriter, r *http.Request, operation, message string, err error) {
	slog.ErrorContext(r.Context(), operation+": "+message, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": message})
}

// indexWriteTarget is the (schema, toolkit) pair every handler below needs,
// resolved once with a single 400 for either half being unusable.
type indexWriteTarget struct {
	schema    string
	toolkitID int64
}

func resolveIndexWriteTarget(w http.ResponseWriter, r *http.Request) (indexWriteTarget, bool) {
	schema, err := tenantSchemaName(chi.URLParam(r, "projectID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return indexWriteTarget{}, false
	}
	toolkitID, err := toolkitRowID(chi.URLParam(r, "toolkitID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return indexWriteTarget{}, false
	}
	return indexWriteTarget{schema: schema, toolkitID: toolkitID}, true
}

// ─────────────────────────────── DELETE ───────────────────────────────

// IndexMetaDelete removes an index and everything that referred to it: every
// index_meta row carrying the same name under this toolkit, and the index's
// entry in the toolkit's `meta.indexes_meta` schedule map.
//
// Both halves matter and both are re-read by
// TestIndexMetaDeleteRemovesTheRowAndItsSchedule. Deleting only the row would
// leave a schedule pointing at an index that no longer exists — legacy runs
// `clean_up_schedule_in_toolkit` for exactly this reason, and the client's
// `useDeleteIndexItemMutation` mirrors the same cleanup locally by calling
// `removeToolkitSchedule(indexName)` on success. If the server kept the
// schedule, a reload would resurrect it in the UI.
//
// The client sends `{"is_hidden": true}` in the request body. It is
// deliberately ignored: there is no hidden/soft-delete column on this table
// and legacy ignores the flag too (its DELETE is unconditional). Honouring it
// would require inventing a soft-delete concept that no read path filters on,
// which is how a "deleted" row comes back.
//
// 200 `{"ok":true}` rather than the stub's 204, matching legacy.
func (h *Handler) IndexMetaDelete(w http.ResponseWriter, r *http.Request) {
	target, ok := resolveIndexWriteTarget(w, r)
	if !ok {
		return
	}
	indexMetaID := chi.URLParam(r, "indexMetaID")
	ctx := r.Context()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeIndexInternalError(w, r, "index_meta_delete", "failed to delete the index", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The name is read from the addressed row and then used as the delete key,
	// so a caller can only ever delete indexes of the toolkit it named.
	var indexName string
	err = tx.QueryRow(ctx,
		fmt.Sprintf(`SELECT name FROM %s.index_meta WHERE id = $1 AND toolkit_id = $2`, target.schema),
		indexMetaID, target.toolkitID,
	).Scan(&indexName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": fmt.Sprintf("index_meta %s not found", indexMetaID),
		})
		return
	}
	if err != nil {
		writeIndexInternalError(w, r, "index_meta_delete", "failed to delete the index", err)
		return
	}

	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s.index_meta WHERE toolkit_id = $1 AND name = $2`, target.schema),
		target.toolkitID, indexName,
	); err != nil {
		writeIndexInternalError(w, r, "index_meta_delete", "failed to delete the index", err)
		return
	}

	// `- $2` deletes the key from the indexes_meta object. COALESCE covers a
	// toolkit that has never had a schedule (meta NULL, or no indexes_meta).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.elitea_tools
		   SET meta = jsonb_set(
		           COALESCE(meta, '{}'::jsonb),
		           '{indexes_meta}',
		           COALESCE(meta->'indexes_meta', '{}'::jsonb) - $2::text)
		 WHERE id = $1`, target.schema),
		target.toolkitID, indexName,
	); err != nil {
		writeIndexInternalError(w, r, "index_meta_delete", "failed to delete the index", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeIndexInternalError(w, r, "index_meta_delete", "failed to delete the index", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─────────────────────────────── PATCH ────────────────────────────────

// indexScheduleUpdate is the PATCH body the web client sends. Only `timezone`
// is always present; the rest are whatever the schedule modal changed.
type indexScheduleUpdate struct {
	Cron        *string `json:"cron"`
	Enabled     *bool   `json:"enabled"`
	Credentials any     `json:"credentials"`
	Timezone    string  `json:"timezone"`
	// UserID selects the schedule bucket. The web client never sends it;
	// legacy defaults it to -1 ("all users"); it resolved -1 to the caller
	// only for a toolkit with a private pgvector configuration, a setting
	// that is retired, so the registry runtime keeps -1.
	UserID *int `json:"user_id"`
}

// IndexMetaUpdate writes one index's schedule into the toolkit's
// `meta.indexes_meta` map.
//
//	meta.indexes_meta[<index name>].schedules[<user id>] =
//	    {cron, enabled, credentials, created_by, timezone, last_run}
//
// The nesting is not incidental: `resolveScheduleData` (IndexActions.tsx:97)
// reads `entry.schedules[currentUserId] ?? entry.schedules[-1]` and
// deliberately does NOT fall back to the bucket itself, so a flat
// `indexes_meta[name] = {cron, ...}` write would round-trip through the API
// and still leave the modal showing its hardcoded default — a silent version
// of the same bug this issue is about.
//
// Responds with the whole `indexes_meta` object, as the legacy runtime does.
func (h *Handler) IndexMetaUpdate(w http.ResponseWriter, r *http.Request) {
	target, ok := resolveIndexWriteTarget(w, r)
	if !ok {
		return
	}
	// Named {indexMetaID} by the route, carries the index NAME — see the file
	// header. It is the key `getIndexSchedule` reads back.
	indexName := chi.URLParam(r, "indexMetaID")
	ctx := r.Context()

	var update indexScheduleUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "index schedule update: " + err.Error(),
		})
		return
	}

	scheduleUserID := defaultScheduleUserID
	if update.UserID != nil {
		scheduleUserID = *update.UserID
	}
	createdBy := ""
	if user, found := auth.UserFromContext(ctx); found {
		createdBy = user.ID
	}

	indexesMeta, err := h.writeIndexSchedule(ctx, target, indexName, scheduleUserID, createdBy, update)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Toolkit not found"})
		return
	case err != nil:
		writeIndexInternalError(w, r, "index_meta_update", "failed to update the index schedule", err)
		return
	}
	writeJSON(w, http.StatusOK, indexesMeta)
}

// writeIndexSchedule does the read-modify-write inside one transaction. The
// row is locked FOR UPDATE because the whole `meta` document is rewritten:
// two concurrent schedule saves for different indexes of the same toolkit
// would otherwise last-write-wins one of them away.
func (h *Handler) writeIndexSchedule(
	ctx context.Context,
	target indexWriteTarget,
	indexName string,
	scheduleUserID int,
	createdBy string,
	update indexScheduleUpdate,
) (map[string]any, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rawMeta []byte
	if err := tx.QueryRow(ctx,
		fmt.Sprintf(`SELECT COALESCE(meta, '{}'::jsonb) FROM %s.elitea_tools WHERE id = $1 FOR UPDATE`, target.schema),
		target.toolkitID,
	).Scan(&rawMeta); err != nil {
		return nil, err
	}

	meta := map[string]any{}
	if len(rawMeta) > 0 {
		if err := json.Unmarshal(rawMeta, &meta); err != nil {
			return nil, err
		}
	}

	indexesMeta, _ := meta["indexes_meta"].(map[string]any)
	if indexesMeta == nil {
		indexesMeta = map[string]any{}
	}
	entry, _ := indexesMeta[indexName].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	schedules, _ := entry["schedules"].(map[string]any)
	if schedules == nil {
		schedules = map[string]any{}
	}

	// Absent fields keep their stored value: the modal submits `{...scheduleData,
	// cron, credentials}` but the enable/disable toggle submits `{enabled}`
	// alone, so a blind overwrite would clear the cron whenever the user
	// toggled the schedule off and on.
	previous, _ := schedules[strconv.Itoa(scheduleUserID)].(map[string]any)
	schedule := map[string]any{}
	for key, value := range previous {
		schedule[key] = value
	}
	if update.Cron != nil {
		schedule["cron"] = *update.Cron
	}
	if update.Enabled != nil {
		schedule["enabled"] = *update.Enabled
	}
	if update.Credentials != nil {
		schedule["credentials"] = update.Credentials
	}
	if update.Timezone != "" {
		schedule["timezone"] = update.Timezone
	}
	schedule["created_by"] = createdBy
	schedule["last_run"] = time.Now().UTC().Format(time.RFC3339)

	schedules[strconv.Itoa(scheduleUserID)] = schedule
	entry["schedules"] = schedules
	indexesMeta[indexName] = entry
	meta["indexes_meta"] = indexesMeta

	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`UPDATE %s.elitea_tools SET meta = $2::jsonb WHERE id = $1`, target.schema),
		target.toolkitID, string(encoded),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return indexesMeta, nil
}

// ─────────────────────────────── CANCEL ───────────────────────────────

// IndexCancel records that a running index should stop.
//
// READ THIS BEFORE BELIEVING THE NAME. What this handler does and does not do
// differ, and the difference is the whole reason it is documented here rather
// than left to look like a full cancel:
//
//   - It DOES transition the addressed `index_meta` row to `cancelled`,
//     durably, guarded on the caller naming the task id the row is actually
//     running. That is a real database write with a real re-read in
//     TestIndexCancelTransitionsARunningRowAndRejectsAStaleTaskID.
//   - It does NOT signal any worker. In this router generation there is no
//     index task registry to signal: index runs are dispatched through
//     `test_toolkit_tool` → indexersvc, whose only cancel RPC
//     (`pipeline_cancel`) belongs to the pipelines runner, and this handler's
//     dependencies are a pool and a ToolTester. The legacy runtime's equivalent calls
//     `self.module.task_node.stop_task(task_id)` — best-effort even there,
//     and there is no such node here.
//
// So an in-flight indexing job may keep running and keep writing after a
// successful cancel. That is a smaller lie than the stub's, but it is still
// a gap, and it is stated in the PR body rather than buried.
//
// The two rejections are deliberate, and both replace what the stub reported
// as success:
//
//   - No such index under this toolkit → 404. Nothing was cancelled and the
//     caller should know.
//   - The row is not in a cancellable status → 409. Cancelling a finished
//     index is not a no-op success; legacy logs "transitioned no row" here
//     and this generation surfaces it.
//   - The named task id is not the one the row is running → 409. This is the
//     stale-cancel case legacy guards with `meta.cmetadata.get("task_id") ==
//     task_id`: without it, a cancel button left over from a previous run
//     kills the run that replaced it.
//
// 204 with no body on success, as legacy and the stub both returned — the
// client ignores the body either way.
func (h *Handler) IndexCancel(w http.ResponseWriter, r *http.Request) {
	target, ok := resolveIndexWriteTarget(w, r)
	if !ok {
		return
	}
	indexName := chi.URLParam(r, "indexName")
	// The web client interpolates a possibly-absent task id into the path, so
	// the literal string "null" arrives when there is none. Legacy performs
	// the same conversion for the same reason.
	taskID := chi.URLParam(r, "taskID")
	if taskID == "null" || taskID == "undefined" {
		taskID = ""
	}
	ctx := r.Context()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeIndexInternalError(w, r, "index_cancel", "failed to cancel the index run", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rowID int64
	var status string
	var storedTaskID string
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT id, status, COALESCE(meta->>'task_id', '')
		  FROM %s.index_meta
		 WHERE toolkit_id = $1 AND name = $2
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1
		   FOR UPDATE`, target.schema),
		target.toolkitID, indexName,
	).Scan(&rowID, &status, &storedTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": fmt.Sprintf("index %q not found for toolkit %d", indexName, target.toolkitID),
		})
		return
	}
	if err != nil {
		writeIndexInternalError(w, r, "index_cancel", "failed to cancel the index run", err)
		return
	}

	if storedTaskID != taskID {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false,
			"error": fmt.Sprintf(
				"index %q is not running task %q; refusing to cancel a task this index is not running", indexName, taskID),
		})
		return
	}
	if !cancellableIndexStatuses[status] {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("index %q is %s and cannot be cancelled", indexName, status),
		})
		return
	}

	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`UPDATE %s.index_meta SET status = 'cancelled' WHERE id = $1`, target.schema),
		rowID,
	); err != nil {
		writeIndexInternalError(w, r, "index_cancel", "failed to cancel the index run", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeIndexInternalError(w, r, "index_cancel", "failed to cancel the index run", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

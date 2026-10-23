package projects

// Project QUOTA and STATISTICS — the two remaining read/write modules of the
// legacy `projects` plugin's v2 API that this package had no route for.
//
//	legacy/plugins/projects/api/v2/quota.py       GET/PUT <project_id>
//	legacy/plugins/projects/api/v2/statistics.py  GET     <project_id>
//
// Neither carries a `{mode}` segment: both are plain flask_restful Resources
// with `url_params = ['<int:project_id>']`, so the URL is
// /api/v2/projects/{quota,statistics}/{project_id} with no mode in it. That is
// not an omission to be "fixed" here — the existing clients call those exact
// paths.
//
// # The reference's statistics endpoint cannot run
//
// This is the one place where faithful porting is impossible, so it is called
// out rather than quietly diverged from. `statistics.py` does two things that
// raise on any current deployment:
//
//   - `Statistic.to_json()` calls `self.rpc.call.tasks_count(...)`. No legacy
//     plugin in legacy/plugins registers a `tasks_count` RPC — grep finds the
//     call site and the Arbiter library's unrelated internals, and no provider.
//   - it then reads `quota["performance_test_runs"]`,
//     `quota["ui_performance_test_runs"]`, `quota["storage_space"]` and
//     `quota["tasks_count"]` out of the project_quota row. project_quota has
//     none of those columns, so the loop raises KeyError on its first key.
//
// So the shape below is a decision, and here it is: every metric the reference
// names is reported as `{current, quota}`; `quota` is null for a metric the
// quota table has no column for, instead of the KeyError; and `tasks_count` is
// dropped entirely, because nothing in this platform counts tasks.
//
// Units are made consistent, which the reference's were not: it reported
// storage_space in MEGABYTES and paired it with a quota column measured in
// GIGABYTES that did not exist. Both sides here are BYTES.
//
// # Quota rollover on read
//
// `check_quota` calls `ProjectQuota.update_time` before comparing, which is a
// WRITE on a GET: after 30 days it advances the window and zeroes the counters.
// That is ported rather than dropped — it is the only thing that ever resets
// them, and a quota check that never rolled over would deny forever once a
// project hit its ceiling. It fires only on the `?quota=` form, exactly as the
// reference does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// quotaWindow is the rollover period `ProjectQuota.update_time` advances by:
// 2592000 seconds, i.e. 30 days. Not a calendar month — the reference adds a
// fixed number of seconds, and a project's window therefore drifts relative to
// the calendar. Kept as-is so a deployment's counters reset when its operators
// expect them to.
const quotaWindow = 2592000 * time.Second

// rollingCounters are the statistic columns `update_time` zeroes when the
// window advances. `tasks_executions` and `public_pool_workers` are NOT in the
// reference's list and are not reset here either — they are lifetime counters.
var rollingCounters = []string{
	"vuh_used", "dast_scans", "sast_scans", "performance_test_runs", "ui_performance_test_runs",
}

// checkableQuotas are the names `?quota=` accepts: a metric is checkable only
// where BOTH tables have a column for it, since `check_quota` indexes the quota
// row and the statistic row with the same key. The reference has no allow-list
// and raises KeyError on anything else; this answers 400 and names the two.
var checkableQuotas = map[string]struct{}{
	"dast_scans": {}, "sast_scans": {},
}

// quotaRow is centry.project_quota as the reference's `to_json` serializes it.
// `id` is included because `to_json` emits every column and existing clients
// were free to read it.
type quotaRow struct {
	ID                     int     `json:"id"`
	ProjectID              int     `json:"project_id"`
	DataRetentionLimit     *int    `json:"data_retention_limit"`
	TestDurationLimit      *int    `json:"test_duration_limit"`
	CPULimit               *int    `json:"cpu_limit"`
	MemoryLimit            *int    `json:"memory_limit"`
	LastUpdateTime         *string `json:"last_update_time"`
	DastScans              *int    `json:"dast_scans"`
	SastScans              *int    `json:"sast_scans"`
	VCUHardLimit           *int    `json:"vcu_hard_limit"`
	VCUSoftLimit           *int    `json:"vcu_soft_limit"`
	VCULimitTotalBlock     bool    `json:"vcu_limit_total_block"`
	StorageHardLimit       *int    `json:"storage_hard_limit"`
	StorageSoftLimit       *int    `json:"storage_soft_limit"`
	StorageLimitTotalBlock bool    `json:"storage_limit_total_block"`
}

const quotaSelect = `
SELECT id, project_id, data_retention_limit, test_duration_limit, cpu_limit, memory_limit,
       to_char(last_update_time, 'YYYY-MM-DD"T"HH24:MI:SS'), dast_scans, sast_scans,
       vcu_hard_limit, vcu_soft_limit, vcu_limit_total_block,
       storage_hard_limit, storage_soft_limit, storage_limit_total_block
FROM centry.project_quota WHERE project_id = $1`

// errNoQuotaRow marks a project with no quota row. The reference answers `null`
// with a 200 in that case (its `.first()` returns None and `.to_json()` is
// called on it — actually raising) or 500; either way "there is no quota" is
// reported as 404 here, so a client can tell it apart from "the quota is empty".
var errNoQuotaRow = errors.New("projects: no quota row")

func (h *Handler) readQuota(ctx context.Context, projectID int) (quotaRow, error) {
	var row quotaRow
	err := h.pool.QueryRow(ctx, quotaSelect, projectID).Scan(
		&row.ID, &row.ProjectID, &row.DataRetentionLimit, &row.TestDurationLimit,
		&row.CPULimit, &row.MemoryLimit, &row.LastUpdateTime, &row.DastScans, &row.SastScans,
		&row.VCUHardLimit, &row.VCUSoftLimit, &row.VCULimitTotalBlock,
		&row.StorageHardLimit, &row.StorageSoftLimit, &row.StorageLimitTotalBlock,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return quotaRow{}, errNoQuotaRow
	}
	if err != nil {
		return quotaRow{}, fmt.Errorf("projects: read quota: %w", err)
	}
	return row, nil
}

func (h *Handler) projectExists(ctx context.Context, projectID int) (bool, error) {
	var exists bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("projects: resolve project: %w", err)
	}
	return exists, nil
}

/* ── GET the quota ─────────────────────────────────────────────────────── */

// GetQuota serves `GET /api/v2/projects/quota/{projectID}`.
//
// Without `?quota=` it answers the quota row. With it, it answers the bare
// JSON boolean "is this project still under that quota?" — which is what
// `check_quota_json` returns, and which the reference cannot actually send:
// Flask's `make_response(True, 200)` raises on a bare bool. So this branch is
// reachable here and is not there.
func (h *Handler) GetQuota(w http.ResponseWriter, r *http.Request) {
	projectID, ok := quotaPathProject(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	exists, err := h.projectExists(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project was not found"})
		return
	}

	metric := r.URL.Query().Get("quota")
	if metric == "" {
		row, err := h.readQuota(ctx, projectID)
		if errors.Is(err, errNoQuotaRow) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project quota was not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
			return
		}
		writeJSON(w, http.StatusOK, row)
		return
	}

	if _, allowed := checkableQuotas[metric]; !allowed {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "quota must be one of dast_scans, sast_scans",
		})
		return
	}

	within, err := h.checkQuota(ctx, projectID, metric)
	if errors.Is(err, errNoQuotaRow) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project quota was not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, within)
}

// checkQuota is `ProjectQuota.check_quota`: roll the window over if it is due,
// then report whether the project is still under the named ceiling. -1 is the
// unlimited sentinel, and a NULL ceiling is treated the same way — the
// reference's `== -1` comparison against None is False, so it would fall
// through to compare a counter against None and raise.
//
// The rollover and the comparison run in ONE transaction. Split across two, a
// reset that committed and a comparison that then read the pre-reset counters
// would deny a project that had just been given a fresh window.
func (h *Handler) checkQuota(ctx context.Context, projectID int, metric string) (bool, error) {
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("projects: begin quota check: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// SELECT ... FOR UPDATE so two concurrent checks cannot both decide the
	// window is due and advance it twice.
	var lastUpdate *time.Time
	var ceiling *int
	err = transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT last_update_time, %s FROM centry.project_quota WHERE project_id = $1 FOR UPDATE`, metric),
		projectID).Scan(&lastUpdate, &ceiling)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNoQuotaRow
	}
	if err != nil {
		return false, fmt.Errorf("projects: read quota ceiling: %w", err)
	}

	if err := rollQuotaWindow(ctx, transaction, projectID, lastUpdate); err != nil {
		return false, err
	}

	if ceiling == nil || *ceiling == -1 {
		// Unlimited. Commit anyway: the window may have just been advanced,
		// and that reset has to survive whether or not there is a ceiling.
		if err := transaction.Commit(ctx); err != nil {
			return false, fmt.Errorf("projects: commit quota check: %w", err)
		}
		return true, nil
	}

	var used *int
	err = transaction.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM centry.statistic WHERE project_id = $1`, metric), projectID).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		// No counters recorded is no consumption, so the project is under.
		if err := transaction.Commit(ctx); err != nil {
			return false, fmt.Errorf("projects: commit quota check: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("projects: read quota usage: %w", err)
	}

	within := used == nil || *used < *ceiling
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("projects: commit quota check: %w", err)
	}
	return within, nil
}

// rollQuotaWindow is `ProjectQuota.update_time`.
//
// The advance is `last + 30 days`, NOT `now`: a project whose window expired
// two months ago catches up one window per check rather than losing the
// intervening ones. That is the reference's arithmetic and it is deliberate
// there too.
func rollQuotaWindow(ctx context.Context, transaction pgx.Tx, projectID int, lastUpdate *time.Time) error {
	if lastUpdate == nil {
		_, err := transaction.Exec(ctx,
			`UPDATE centry.project_quota SET last_update_time = (now() AT TIME ZONE 'utc') WHERE project_id = $1`,
			projectID)
		if err != nil {
			return fmt.Errorf("projects: stamp quota window: %w", err)
		}
		return nil
	}
	if time.Since(*lastUpdate) <= quotaWindow {
		return nil
	}

	if _, err := transaction.Exec(ctx, `
UPDATE centry.project_quota
SET last_update_time = last_update_time + INTERVAL '2592000 seconds'
WHERE project_id = $1`, projectID); err != nil {
		return fmt.Errorf("projects: advance quota window: %w", err)
	}

	assignments := ""
	for index, column := range rollingCounters {
		if index > 0 {
			assignments += ", "
		}
		assignments += column + " = 0"
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		`UPDATE centry.statistic SET %s WHERE project_id = $1`, assignments), projectID); err != nil {
		return fmt.Errorf("projects: reset quota counters: %w", err)
	}
	return nil
}

/* ── PUT the quota ─────────────────────────────────────────────────────── */

// quotaUpdate is the body both `usage_type` branches accept; each reads only
// its own three fields.
type quotaUpdate struct {
	VCUHardLimit           *int  `json:"vcu_hard_limit"`
	VCUSoftLimit           *int  `json:"vcu_soft_limit"`
	VCULimitTotalBlock     *bool `json:"vcu_limit_total_block"`
	StorageHardLimit       *int  `json:"storage_hard_limit"`
	StorageSoftLimit       *int  `json:"storage_soft_limit"`
	StorageLimitTotalBlock *bool `json:"storage_limit_total_block"`
}

// PutQuota serves `PUT /api/v2/projects/quota/{projectID}?usage_type=vcu|storage`.
//
// Two divergences from the reference, both cases where it raises rather than
// answers: a missing `usage_type` calls `.lower()` on None (AttributeError →
// 500) and is a 400 here; an unknown one falls off the end of the function and
// returns None, which flask_restful renders as a bare 200 with no body, and is
// a 400 here too. A project with no quota row dereferences None there; it is a
// 404 here.
func (h *Handler) PutQuota(w http.ResponseWriter, r *http.Request) {
	projectID, ok := quotaPathProject(w, r)
	if !ok {
		return
	}

	usageType := r.URL.Query().Get("usage_type")
	var columns [3]string
	switch usageType {
	case "vcu":
		columns = [3]string{"vcu_hard_limit", "vcu_soft_limit", "vcu_limit_total_block"}
	case "storage":
		columns = [3]string{"storage_hard_limit", "storage_soft_limit", "storage_limit_total_block"}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "usage_type must be 'vcu' or 'storage'",
		})
		return
	}

	var body quotaUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Can not validate data"})
		return
	}

	hard, soft, block := body.VCUHardLimit, body.VCUSoftLimit, body.VCULimitTotalBlock
	if usageType == "storage" {
		hard, soft, block = body.StorageHardLimit, body.StorageSoftLimit, body.StorageLimitTotalBlock
	}
	// The reference's `update_*_limits` defaults the block flag to False when
	// the body omits it, and assigns hard/soft unconditionally — so an omitted
	// limit CLEARS it. That is faithful: the form always sends all three, and
	// treating omission as "leave alone" would make it impossible to clear one.
	totalBlock := false
	if block != nil {
		totalBlock = *block
	}

	ctx := r.Context()
	tag, err := h.pool.Exec(ctx, fmt.Sprintf(`
UPDATE centry.project_quota SET %s = $2, %s = $3, %s = $4 WHERE project_id = $1`,
		columns[0], columns[1], columns[2]), projectID, hard, soft, totalBlock)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project quota was not found"})
		return
	}

	row, err := h.readQuota(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

/* ── statistics ────────────────────────────────────────────────────────── */

// statisticEntry is one `{current, quota}` pair. `quota` is a pointer so a
// metric the quota table has no column for reports null rather than a number
// nobody configured — the KeyError the reference raises instead.
type statisticEntry struct {
	Current int64  `json:"current"`
	Quota   *int64 `json:"quota"`
}

// GetStatistics serves `GET /api/v2/projects/statistics/{projectID}`.
func (h *Handler) GetStatistics(w http.ResponseWriter, r *http.Request) {
	projectID, ok := quotaPathProject(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	exists, err := h.projectExists(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project was not found"})
		return
	}

	var (
		performance, uiPerformance, sast, dast, tasks int64
	)
	// A project with no counter row has made no runs, which is 0 — not an
	// error and not a 404. The reference dereferences the missing row.
	err = h.pool.QueryRow(ctx, `
SELECT COALESCE(performance_test_runs, 0), COALESCE(ui_performance_test_runs, 0),
       COALESCE(sast_scans, 0), COALESCE(dast_scans, 0), COALESCE(tasks_executions, 0)
FROM centry.statistic WHERE project_id = $1`, projectID).Scan(
		&performance, &uiPerformance, &sast, &dast, &tasks)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}

	var sastQuota, dastQuota, retentionQuota, storageHardLimitGB *int64
	err = h.pool.QueryRow(ctx, `
SELECT sast_scans, dast_scans, data_retention_limit, storage_hard_limit
FROM centry.project_quota WHERE project_id = $1`, projectID).Scan(
		&sastQuota, &dastQuota, &retentionQuota, &storageHardLimitGB)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}

	storageBytes, err := h.projectStorageBytes(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]statisticEntry{
		"performance_test_runs":    {Current: performance, Quota: nil},
		"ui_performance_test_runs": {Current: uiPerformance, Quota: nil},
		"sast_scans":               {Current: sast, Quota: unlimitedToNil(sastQuota)},
		"dast_scans":               {Current: dast, Quota: unlimitedToNil(dastQuota)},
		"tasks_executions":         {Current: tasks, Quota: nil},
		"storage_space":            {Current: storageBytes, Quota: gigabytesToBytes(storageHardLimitGB)},
		// The reference hardcodes the current side of this one to 0: nothing
		// measures retention, only configures it.
		"data_retention_limit": {Current: 0, Quota: retentionQuota},
	})
}

// projectStorageBytes is the live size of everything the project has stored —
// the Go equivalent of the reference's walk over the project's MinIO buckets,
// asked of the artifact-storage tables in one query instead of one listing per
// bucket.
//
// The relation check is not defensive noise: elitea_storage comes from
// migrations/shared/0057_artifact_storage.sql, and a database restored from a
// pre-storage dump does not have it. PostgreSQL parses a whole statement before
// executing it, so a CASE guard inside the query would still fail — the check
// has to be its own round trip.
func (h *Handler) projectStorageBytes(ctx context.Context, projectID int) (int64, error) {
	var present bool
	if err := h.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_storage.objects') IS NOT NULL
		    AND to_regclass('elitea_storage.buckets') IS NOT NULL`).Scan(&present); err != nil {
		return 0, fmt.Errorf("projects: probe storage tables: %w", err)
	}
	if !present {
		return 0, nil
	}

	var total int64
	if err := h.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(object.byte_length), 0)
FROM elitea_storage.objects object
JOIN elitea_storage.buckets bucket ON bucket.id = object.bucket_id
WHERE bucket.project_id = $1 AND bucket.deleted_at IS NULL`, projectID).Scan(&total); err != nil {
		return 0, fmt.Errorf("projects: sum project storage: %w", err)
	}
	return total, nil
}

// unlimitedToNil maps the quota tables' -1 sentinel onto JSON null, so a client
// does not have to know that -1 means "no ceiling" to render the row.
func unlimitedToNil(value *int64) *int64 {
	if value == nil || *value == -1 {
		return nil
	}
	return value
}

// gigabytesToBytes converts storage_hard_limit, which the reference's
// `storage_hard_limit_in_bytes` property multiplies by 1_000_000_000.
func gigabytesToBytes(gigabytes *int64) *int64 {
	if gigabytes == nil || *gigabytes < 0 {
		return nil
	}
	bytes := *gigabytes * 1_000_000_000
	return &bytes
}

func quotaPathProject(w http.ResponseWriter, r *http.Request) (int, bool) {
	value, err := parseInt32(chi.URLParam(r, "projectID"))
	if err != nil || value <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "project id must be a positive integer",
		})
		return 0, false
	}
	return int(value), true
}

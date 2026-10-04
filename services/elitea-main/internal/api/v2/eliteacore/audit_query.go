package eliteacore

// Query construction for the four admin audit-trail reads (unit A14, #200).
//
// The wire contract is not invented: it mirrors the pylon handlers the existing
// admin_ui client already speaks to —
//   legacy/plugins/elitea_core/api/v2/audit.py
//   legacy/plugins/elitea_core/api/v2/audit_traces.py
//   legacy/plugins/elitea_core/api/v2/audit_heatmap.py
//   legacy/plugins/elitea_core/api/v2/audit_trace_heatmap.py
// — same paths, same query parameters, same body keys.
//
// The rows come from `centry.audit_events`. Nothing in THIS file writes: the
// audit surface is read-only, by design. The producer is internal/audit, driven
// by the middleware in internal/api/middleware/audit.go, and it emits the same
// `event_type`/`action` vocabulary these queries filter and group by — the
// legacy tracing plugin's (legacy/plugins/tracing/utils/audit_processor.py).
//
// ## Every filter is a bound parameter, and every identifier is allow-listed
//
// `sort_by` / `sort_order` reach an ORDER BY, and the heatmap's bucket width
// reaches an arithmetic expression, so neither can be taken from the request
// verbatim. Both are looked up in a fixed map/table and an unknown value falls
// back to the default — the same shape as `sortableUserColumns` in
// internal/api/v2/admin/users.go. Everything else is a `$n` placeholder.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// auditTable is the one table this file reads. Qualified with the shared
// `centry` schema, like every other central table in this service
// (centry.project, centry.secrets_key, centry.notifications, …).
const auditTable = `centry.audit_events`

// auditFilters is the parsed query string, shared by all four endpoints
// because pylon accepts the same filter set on all four.
type auditFilters struct {
	search      string
	eventTypes  []string
	httpMethod  string
	onlyErrors  bool
	userID      *int64
	projectID   *int64
	traceID     string
	entityName  string
	action      string
	dateFrom    *time.Time
	dateTo      *time.Time
	durationMin *float64
	durationMax *float64
}

func parseAuditFilters(r *http.Request) auditFilters {
	query := r.URL.Query()
	return auditFilters{
		search:      query.Get("search"),
		eventTypes:  splitEventTypes(query.Get("event_type")),
		httpMethod:  strings.ToUpper(query.Get("http_method")),
		onlyErrors:  strings.EqualFold(query.Get("is_error"), "true"),
		userID:      optionalInt(query.Get("user_id")),
		projectID:   optionalInt(query.Get("project_id")),
		traceID:     query.Get("trace_id"),
		entityName:  query.Get("entity_name"),
		action:      query.Get("action"),
		dateFrom:    optionalTime(query.Get("date_from")),
		dateTo:      optionalTime(query.Get("date_to")),
		durationMin: optionalFloat(query.Get("duration_min")),
		durationMax: optionalFloat(query.Get("duration_max")),
	}
}

// parseAuditRequest is parseAuditFilters for a handler. A window whose
// date_from is later than its date_to is answered with 400
// invalid_date_range, and ok is false so the handler returns (legacy issue
// 6738). Before this the reversed window reached the SQL as it was, every
// range condition matched nothing, and the page showed an empty audit trail
// that looked like a quiet period.
func parseAuditRequest(w http.ResponseWriter, r *http.Request) (auditFilters, bool) {
	filters := parseAuditFilters(r)
	if reversedDateRange(filters.dateFrom, filters.dateTo) {
		writeInvalidDateRange(w)
		return auditFilters{}, false
	}
	return filters, true
}

// reversedDateRange reports a window whose start is later than its end. A
// missing bound is an open window, never a reversed one, and an equal start and
// end is an empty window, not an invalid one.
func reversedDateRange(from, to *time.Time) bool {
	return from != nil && to != nil && from.After(*to)
}

// writeInvalidDateRange answers a reversed window. The code is the one the
// analytics endpoints use for the same mistake, so one client check covers both.
func writeInvalidDateRange(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": "date_from must not be later than date_to",
		"code":  "invalid_date_range",
	})
}

// splitEventTypes accepts pylon's comma-separated list. The page always sends
// one — the active tab's whole set ("api,socketio,rpc,agent,tool,llm") when no
// type is chosen — so a single-element result is the exception, not the rule.
func splitEventTypes(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	types := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			types = append(types, trimmed)
		}
	}
	return types
}

func optionalInt(raw string) *int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

func optionalFloat(raw string) *float64 {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return &value
}

// optionalTime accepts what `Date.prototype.toISOString()` emits (the page's
// only format) as well as the offset-bearing and naive forms pylon's
// `datetime.fromisoformat` took, so an existing admin_ui client keeps working.
func optionalTime(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return &parsed
		}
	}
	return nil
}

// argList accumulates bound parameters and hands out their `$n` placeholders,
// so a condition can never disagree with its argument's position.
type argList struct {
	values []any
}

func (a *argList) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// conditions renders the filter set as SQL predicates against `alias`.
//
// `includeDuration` is false for the trace endpoints: there, `duration_min` /
// `duration_max` describe the duration of the whole TRACE, which only exists
// after the per-trace aggregation, so applying them to individual spans here
// would silently filter on the wrong quantity.
func (f auditFilters) conditions(alias string, args *argList, includeDuration bool) []string {
	var where []string
	if f.search != "" {
		pattern := args.add("%" + f.search + "%")
		where = append(where, fmt.Sprintf(
			"(%[1]s.action ILIKE %[2]s OR %[1]s.tool_name ILIKE %[2]s OR %[1]s.user_email ILIKE %[2]s OR %[1]s.model_name ILIKE %[2]s)",
			alias, pattern))
	}
	if len(f.eventTypes) > 0 {
		where = append(where, fmt.Sprintf("%s.event_type = ANY(%s)", alias, args.add(f.eventTypes)))
	}
	if f.httpMethod != "" {
		where = append(where, fmt.Sprintf("%s.http_method = %s", alias, args.add(f.httpMethod)))
	}
	if f.onlyErrors {
		where = append(where, fmt.Sprintf("%s.is_error IS TRUE", alias))
	}
	if f.userID != nil {
		where = append(where, fmt.Sprintf("%s.user_id = %s", alias, args.add(*f.userID)))
	}
	if f.projectID != nil {
		where = append(where, fmt.Sprintf("%s.project_id = %s", alias, args.add(*f.projectID)))
	}
	if f.traceID != "" {
		where = append(where, fmt.Sprintf("%s.trace_id = %s", alias, args.add(f.traceID)))
	}
	if f.entityName != "" {
		where = append(where, fmt.Sprintf("%s.entity_name ILIKE %s", alias, args.add("%"+f.entityName+"%")))
	}
	if f.action != "" {
		where = append(where, fmt.Sprintf("%s.action ILIKE %s", alias, args.add("%"+f.action+"%")))
	}
	where = append(where, f.rangeConditions(alias, args, includeDuration)...)
	return where
}

func (f auditFilters) rangeConditions(alias string, args *argList, includeDuration bool) []string {
	var where []string
	if f.dateFrom != nil {
		where = append(where, fmt.Sprintf("%s.timestamp >= %s", alias, args.add(*f.dateFrom)))
	}
	if f.dateTo != nil {
		where = append(where, fmt.Sprintf("%s.timestamp <= %s", alias, args.add(*f.dateTo)))
	}
	if includeDuration && f.durationMin != nil {
		where = append(where, fmt.Sprintf("%s.duration_ms >= %s", alias, args.add(*f.durationMin)))
	}
	if includeDuration && f.durationMax != nil {
		where = append(where, fmt.Sprintf("%s.duration_ms < %s", alias, args.add(*f.durationMax)))
	}
	return where
}

// whereClause joins predicates, or renders TRUE when there are none, so a
// caller never has to branch on "did any filter apply".
func whereClause(conditions []string) string {
	if len(conditions) == 0 {
		return "TRUE"
	}
	return strings.Join(conditions, " AND ")
}

/* ── ordering ───────────────────────────────────────────────────────────── */

// sortableSpanColumns mirrors audit.py's `_SORT_WHITELIST`. The value is
// interpolated into the ORDER BY, so this map is also what keeps the query
// injection-free; an unknown `sort_by` falls back to `timestamp`.
var sortableSpanColumns = map[string]string{
	"timestamp":   "e.timestamp",
	"user_email":  "e.user_email",
	"event_type":  "e.event_type",
	"action":      "e.action",
	"http_method": "e.http_method",
	"status_code": "e.status_code",
	"duration_ms": "e.duration_ms",
	"project_id":  "e.project_id",
	"entity_name": "e.entity_name",
}

// sortableTraceColumns mirrors audit_traces.py's `_SORT_WHITELIST`.
var sortableTraceColumns = map[string]string{
	"start_time":  "a.start_time",
	"duration_ms": "a.duration_ms",
	"span_count":  "a.span_count",
	"user_email":  "a.user_email",
	"project_id":  "a.project_id",
}

// orderBy resolves `sort_by`/`sort_order` against an allow-list and appends
// `tiebreak` as a final, always-unique sort key.
//
// The tiebreaker is a DELIBERATE addition to the pylon original, not a porting
// slip. `ORDER BY timestamp DESC` alone is not a total order — audit rows are
// written in bursts and ties are common — and PostgreSQL is free to return tied
// rows in a different order for each LIMIT/OFFSET page. That makes rows repeat
// on one page and vanish from another while paging through an unchanged table.
func orderBy(sortBy, sortOrder string, columns map[string]string, fallback, tiebreak string) string {
	column, ok := columns[sortBy]
	if !ok {
		column = columns[fallback]
	}
	direction := "DESC"
	if strings.EqualFold(sortOrder, "asc") {
		direction = "ASC"
	}
	// NULLS LAST in both directions: a NULL duration or status code is "no
	// value", and sorting it above every real one buries the rows being sought.
	return fmt.Sprintf("%s %s NULLS LAST, %s %s", column, direction, tiebreak, direction)
}

/* ── heatmap bucketing ──────────────────────────────────────────────────── */

// heatmapBands are the duration bands, indexed exactly as the SQL CASE below
// and as audit_heatmap.py's `_BAND_LABELS`. The client maps these labels back
// to [min,max) millisecond bounds when a cell is clicked, so the strings are
// part of the contract.
var heatmapBands = []string{"<10ms", "10-100ms", "100ms-1s", "1-10s", ">10s"}

// bandExpression buckets `column` into heatmapBands' indices. `column` is
// always a literal built here, never request data.
func bandExpression(column string) string {
	return fmt.Sprintf(
		"CASE WHEN %[1]s < 10 THEN 0 WHEN %[1]s < 100 THEN 1 WHEN %[1]s < 1000 THEN 2 WHEN %[1]s < 10000 THEN 3 ELSE 4 END",
		column)
}

type bucketInterval struct {
	seconds int64
	label   string
}

// heatmapIntervals mirrors audit_heatmap.py's `_INTERVAL_TABLE`: the first
// entry whose threshold covers the requested range wins.
var heatmapIntervals = []struct {
	thresholdSeconds int64
	interval         bucketInterval
}{
	{3600, bucketInterval{60, "1min"}},
	{6 * 3600, bucketInterval{300, "5min"}},
	{24 * 3600, bucketInterval{900, "15min"}},
	{7 * 86400, bucketInterval{3600, "1h"}},
	{30 * 86400, bucketInterval{14400, "4h"}},
}

var defaultHeatmapInterval = bucketInterval{86400, "1d"}

func pickInterval(rangeSeconds int64) bucketInterval {
	for _, candidate := range heatmapIntervals {
		if rangeSeconds <= candidate.thresholdSeconds {
			return candidate.interval
		}
	}
	return defaultHeatmapInterval
}

// maxHeatmapBuckets caps the response size. Past 30 days the bucket width stops
// growing (1 day), so the slot count grows without bound with the requested
// range — a 500-year `date_from` would otherwise ask this handler to build ~1M
// slots × 5 bands in memory. pylon has no such cap.
const maxHeatmapBuckets = 5000

// bucketExpression floors `column`'s epoch seconds to the interval grid.
// `interval.seconds` comes from the fixed table above, never from the request.
func bucketExpression(column string, interval bucketInterval) string {
	return fmt.Sprintf("(FLOOR(EXTRACT(EPOCH FROM %s) / %d) * %d)::bigint", column, interval.seconds, interval.seconds)
}

// timeSlots enumerates the full requested range on the bucket grid, so a period
// with no events still renders as an empty column rather than being skipped and
// silently compressing the time axis.
func timeSlots(from, to time.Time, interval bucketInterval) []int64 {
	first := floorToInterval(from.Unix(), interval.seconds)
	last := floorToInterval(to.Unix(), interval.seconds)
	slots := make([]int64, 0, (last-first)/interval.seconds+1)
	for slot := first; slot <= last; slot += interval.seconds {
		slots = append(slots, slot)
	}
	return slots
}

// floorToInterval rounds towards negative infinity, unlike Go's truncating
// integer division, so a pre-1970 timestamp lands in the bucket that CONTAINS
// it rather than the one after it — matching the SQL FLOOR() above.
func floorToInterval(epoch, interval int64) int64 {
	bucket := epoch / interval
	if epoch%interval != 0 && epoch < 0 {
		bucket--
	}
	return bucket * interval
}

// heatmapSeries renders the counted cells as the nivo-shaped series array the
// client consumes: bands from slowest to fastest (so ">10s" is the top row),
// each with one point per time slot. A slot with no events carries `y: null`,
// which the client renders as an empty cell rather than as a zero.
func heatmapSeries(counts map[[2]int64]int64, slots []int64) []map[string]any {
	series := make([]map[string]any, 0, len(heatmapBands))
	for band := len(heatmapBands) - 1; band >= 0; band-- {
		points := make([]map[string]any, 0, len(slots))
		for _, slot := range slots {
			var value any
			if count, ok := counts[[2]int64{slot, int64(band)}]; ok {
				value = count
			}
			points = append(points, map[string]any{"x": slot, "y": value})
		}
		series = append(series, map[string]any{"id": heatmapBands[band], "data": points})
	}
	return series
}

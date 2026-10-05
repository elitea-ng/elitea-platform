package gateway

// llm_proxy_logs.go — the request log, `GET /api/v2/admin/gateway/logs`.
//
// ## The question this answers and the usage report cannot
//
// `gateway.llm_usage_events` is written from the billing delta, and a billing
// delta rides only a BILLED request. A call refused by a budget, rejected by a
// policy, addressed to a model that does not resolve, or failed upstream is
// absent from it entirely — so the Usage tab can say what a deployment SPENT
// and can never say what FAILED.
//
// `gateway.llm_request_logs` (shared migration 0099) is one row per request the
// gateway served, whatever happened to it. The gateway writes it directly, in
// batches, off the request path; this file only reads.
//
// ## What it cannot show, and why that is deliberate
//
// No prompt, no completion, no upstream error text — the table has no column
// any of them could reach. A prompt is user-authored free text carrying
// whatever was pasted into it, and provider errors routinely quote the
// offending fragment of the request back. So this surface answers "what
// happened, to whom, how often and how slowly" and cannot be made to answer
// "what was in it", which is a property of the schema rather than a rule this
// handler enforces.
//
// An operator who needs the payload of one request has to reproduce it. That is
// the intended trade.
//
// ## Paging is by CURSOR, not by offset
//
// The table is append-mostly and the listing is newest-first, so an offset
// shifts under the reader: rows arriving between page one and page two push
// every later row down, and the operator sees duplicates while missing others.
// A cursor on the monotonic id is stable under concurrent writes, which is the
// normal state of this table.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// logReadTimeout bounds one page. The statement is index-served and bounded, so
// this is a backstop rather than a budget.
const logReadTimeout = 15 * time.Second

// logPageSize is one page of rows.
//
// Fixed rather than caller-chosen: an unbounded `?limit=` on a table that grows
// with traffic is a way for one request to serialise a great deal of it, and the
// screen shows a scrolling list where a bigger page buys nothing.
const logPageSize = 100

// RequestLogRow is one served request, as the admin listing reports it.
type RequestLogRow struct {
	// ID is the cursor. Exposed as a string because it is a BIGSERIAL and a
	// JavaScript number cannot hold the full range — a client that parsed it as
	// a number would start losing precision, silently, at a point no test would
	// reach.
	ID         string `json:"id"`
	OccurredAt string `json:"occurred_at"`
	ProjectID  *int64 `json:"project_id"`
	UserID     *int64 `json:"user_id"`
	Route      string `json:"route"`
	Method     string `json:"method"`
	Status     int    `json:"status"`
	DurationMS int    `json:"duration_ms"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Streaming  bool   `json:"streaming"`
	// ErrorCode is the gateway's own classification. Empty on success.
	ErrorCode        string `json:"error_code"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// logFilters is one request's narrowing, already validated.
type logFilters struct {
	window     string
	interval   string
	projectID  *int64
	model      string
	failedOnly bool
	cursor     *int64
}

// listLogsSQL reads one page.
//
// The predicates are all bound parameters with a NULL short-circuit, so one
// statement serves every combination of filters and there is a single query
// plan to reason about. `id < $cursor` pages backwards through a DESC ordering,
// which is what "older than the last row you saw" means here.
const listLogsSQL = `
	SELECT id::text, occurred_at, project_id, user_id, route, method, status,
	       duration_ms, provider, model, streaming, error_code,
	       prompt_tokens, completion_tokens
	  FROM gateway.llm_request_logs
	 WHERE occurred_at >= now() - $1::interval
	   AND ($2::bigint IS NULL OR project_id = $2::bigint)
	   AND ($3::text = '' OR model = $3::text)
	   AND ($4::boolean = false OR status >= 400)
	   AND ($5::bigint IS NULL OR id < $5::bigint)
	 ORDER BY id DESC
	 LIMIT $6`

// logSummarySQL is the window's shape, over the SAME predicates as the page.
//
// It is a separate statement rather than a count on the page because the page
// is capped: an operator needs "412 requests, 37 of them failed" for the whole
// window, and the page can only ever say "100".
const logSummarySQL = `
	SELECT count(*),
	       count(*) FILTER (WHERE status >= 400),
	       COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY duration_ms), 0),
	       COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)
	  FROM gateway.llm_request_logs
	 WHERE occurred_at >= now() - $1::interval
	   AND ($2::bigint IS NULL OR project_id = $2::bigint)
	   AND ($3::text = '' OR model = $3::text)
	   AND ($4::boolean = false OR status >= 400)`

// Logs serves GET /gateway/logs.
func (h *LLMProxyHandler) Logs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), logReadTimeout)
	defer cancel()

	filters, reason := parseLogFilters(r)
	if reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}

	if h == nil || h.db == nil {
		writeJSON(w, http.StatusOK, emptyLogBody(filters.window,
			"this deployment has no database pool, so the request log cannot be read."))
		return
	}

	rows, err := h.queryLogs(ctx, filters)
	if err != nil {
		// An explained empty page rather than a 5xx, matching the catalogue and
		// usage reads beside it: the screen renders the reason and the rest of
		// the section stays reachable.
		writeJSON(w, http.StatusOK, emptyLogBody(filters.window, err.Error()))
		return
	}

	body := map[string]any{
		"items":  rows,
		"window": filters.window,
		// The cursor for the NEXT page, absent when this page is the last one.
		// Absent rather than null-or-empty so a client cannot accidentally
		// request a page keyed on "".
		"retention_days": requestLogRetentionDays,
	}
	if len(rows) == logPageSize {
		body["next_cursor"] = rows[len(rows)-1].ID
	}

	summary, summaryErr := h.queryLogSummary(ctx, filters)
	body["summary"] = summary
	if summaryErr != nil {
		// Reported separately: the page is still worth showing, and a summary
		// that failed must not render as "0 failures in this window", which is
		// the reassuring reading of a zeroed struct.
		body["summary_error"] = summaryErr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// requestLogRetentionDays restates the gateway's requestlog.RetentionWindow.
//
// Restated, not imported: it lives in an `internal/` package of a different
// module. The number is compiled on both sides — deliberately not configurable,
// so a deployment cannot turn a log into an unbounded table — which makes drift
// a code change that edits one and not the other.
const requestLogRetentionDays = 92

// LogSummary is the window's shape.
type LogSummary struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
	// Latency percentiles in milliseconds. The MEDIAN and p95 rather than a
	// mean: a mean over a mix of streamed and buffered responses is dominated
	// by the streams and describes neither.
	MedianMS int `json:"median_ms"`
	P95MS    int `json:"p95_ms"`
}

func emptyLogBody(window, reason string) map[string]any {
	return map[string]any{
		"items":          []RequestLogRow{},
		"window":         window,
		"summary":        LogSummary{},
		"retention_days": requestLogRetentionDays,
		"error":          reason,
	}
}

// parseLogFilters validates the query string.
//
// An unrecognised window falls back rather than refusing, like the catalogue's:
// the window is a display choice and a mistyped one must not hide the log. A
// malformed project id or cursor IS refused, because both are addressing rather
// than display — silently ignoring them would show the operator a different
// deployment's traffic than the one they asked for.
func parseLogFilters(r *http.Request) (logFilters, string) {
	query := r.URL.Query()
	window, interval := resolveWindow(query.Get("window"))
	filters := logFilters{
		window:     window,
		interval:   strconv.FormatInt(int64(interval/time.Second), 10) + " seconds",
		model:      strings.TrimSpace(query.Get("model")),
		failedOnly: query.Get("failed") == "true",
	}

	if raw := strings.TrimSpace(query.Get("project_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return logFilters{}, "project_id must be a positive integer"
		}
		filters.projectID = &parsed
	}
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return logFilters{}, "cursor must be a positive integer"
		}
		filters.cursor = &parsed
	}
	if len(filters.model) > 128 {
		return logFilters{}, "model is too long"
	}
	return filters, ""
}

func (h *LLMProxyHandler) queryLogs(ctx context.Context, filters logFilters) ([]RequestLogRow, error) {
	rows, err := h.db.Query(ctx, listLogsSQL,
		filters.interval, filters.projectID, filters.model, filters.failedOnly,
		filters.cursor, logPageSize)
	if err != nil {
		return nil, fmt.Errorf("read the request log: %w", err)
	}
	defer rows.Close()

	items := make([]RequestLogRow, 0, logPageSize)
	for rows.Next() {
		var row RequestLogRow
		var occurredAt time.Time
		if scanErr := rows.Scan(&row.ID, &occurredAt, &row.ProjectID, &row.UserID,
			&row.Route, &row.Method, &row.Status, &row.DurationMS, &row.Provider,
			&row.Model, &row.Streaming, &row.ErrorCode,
			&row.PromptTokens, &row.CompletionTokens); scanErr != nil {
			return nil, fmt.Errorf("read the request log: %w", scanErr)
		}
		row.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
		items = append(items, row)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read the request log: %w", rows.Err())
	}
	return items, nil
}

func (h *LLMProxyHandler) queryLogSummary(ctx context.Context, filters logFilters) (LogSummary, error) {
	var summary LogSummary
	err := h.db.QueryRow(ctx, logSummarySQL,
		filters.interval, filters.projectID, filters.model, filters.failedOnly).
		Scan(&summary.Requests, &summary.Failed, &summary.MedianMS, &summary.P95MS)
	if err != nil {
		return LogSummary{}, fmt.Errorf("summarise the request log: %w", err)
	}
	return summary, nil
}

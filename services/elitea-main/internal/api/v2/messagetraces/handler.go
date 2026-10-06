// Package messagetraces serves the two chat execution-step reads: the trace
// pins under an agent's answer, and the full step behind one pin.
//
//	GET /elitea_core/message_traces/prompt_lib/{projectID}/{conversationID}
//	GET /elitea_core/message_trace/prompt_lib/{projectID}/{stepID}
//	    ← legacy/plugins/elitea_core/api/v2/{message_traces,message_trace}.py
//	      and the RPCs behind them, rpc/chat_trace_step.py
//
// # The producer exists
//
// Issue 253 left this open: implement the reads against the execution-trace
// storage, or, if nothing writes step traces yet, land them in the
// agent-execution stack behind whatever schema that work defines. Nothing had
// to be deferred — `<tenant>.chat_message_trace_step` is written today by this
// service's own agent-execution trace projection
// (internal/infra/db/repos/agent_trace.go, which INSERTs and UPDATEs the same
// columns read here on every partial and terminal frame). So these are reads
// over a live table, and the shape below is that table's, not a placeholder.
//
// # Light list, heavy detail
//
// The split is the whole reason there are two endpoints. A conversation's pin
// list asks for every step of every loaded message group at once, so the list
// query selects labels, ordering and the bounded `attrs` sidecar ONLY. The
// heavy columns — tool_inputs, tool_output, text, thinking — are TOASTed
// multi-kilobyte values and are fetched one row at a time, on pin expand. A
// list that selected them would detoast the lot to render chips.
//
// Blank thinking steps are excluded from the list, matching the reference: the
// runtime emits a thinking step with an action but no text as a transition
// marker (a tool-call boundary), `has_visible_content` marks those, and the
// client draws a pin for every row it is handed.
//
// # Scoping
//
// Both reads are confined to the caller's project by the tenant schema they
// run in (`p_<projectID>`, built from the path parameter through
// pgx.Identifier.Sanitize, never interpolated raw), and further:
//
//   - the list joins chat_message_group and filters on conversation_id, so a
//     group id from another conversation contributes nothing;
//   - the detail requires message_group_id and matches on BOTH it and the step
//     id, so a bare step id from a conversation the caller is not reading is a
//     404 rather than a body. That pairing is the reference's, and it is the
//     only thing standing between a numeric step id and every trace in the
//     project — which is why message_group_id is required rather than an
//     optional narrowing filter.
package messagetraces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Permissions the routes are gated on in internal/api/router.go, transcribed
// from the `check_api` declarations of the two pylon handlers. Both are
// DEFAULT-mode, project-scoped, and both are granted to default-mode admin,
// editor and viewer by migrations/shared/0063_trace_and_cost_read_permissions.sql
// — without that grant a project-scoped gate resolves to the empty set and is
// 403-for-everyone on a Go-bootstrapped database.
const (
	// ListPermission gates the conversation-scoped listing (message_traces.py).
	ListPermission = "models.chat.messages.list"
	// DetailPermission gates the single-step read (message_trace.py).
	DetailPermission = "models.chat.messages.details"
)

// Bounds transcribed from rpc/chat_trace_step.py. The list is bounded twice —
// by rows and by how many message groups may be named — because a conversation
// with thousands of groups would otherwise fan one request into an unbounded
// IN list.
const (
	defaultLimit      = 2000
	maxLimit          = 2000
	maxMessageGroupID = 200
)

// stepKinds is the closed set the `kind` filter accepts. An unknown kind is a
// 400 rather than a silent empty page: the reference raises, and a filter that
// quietly matches nothing reads as "this conversation has no tool calls".
var stepKinds = map[string]struct{}{
	"tool_call":     {},
	"thinking_step": {},
}

// Handler serves the two reads over the tenant schema.
type Handler struct {
	pool *pgxpool.Pool
}

// NewHandler builds a Handler over the shared pool.
func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

/* ── list ──────────────────────────────────────────────────────────────── */

// listItem is the light projection: what a resting chip draws, plus what
// orders it. `attrs` is the bounded display sidecar (icon, display name,
// toolkit type) the reference carries so a reloaded chip renders without a
// detail fetch; it never holds the heavy fields.
type listItem struct {
	ID                int64      `json:"id"`
	MessageGroupID    int64      `json:"message_group_id"`
	Kind              string     `json:"kind"`
	ToolName          *string    `json:"tool_name"`
	ParentAgentName   *string    `json:"parent_agent_name"`
	ParentAgentCallID *string    `json:"parent_agent_call_id"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	IsError           bool       `json:"is_error"`
	StepType          *string    `json:"step_type"`
	ModelName         *string    `json:"model_name"`
	FinishReason      *string    `json:"finish_reason"`
	Attrs             any        `json:"attrs"`
}

// detailItem adds the heavy columns to the light projection.
type detailItem struct {
	listItem
	ToolInputs any     `json:"tool_inputs"`
	ToolOutput *string `json:"tool_output"`
	Text       *string `json:"text"`
	Thinking   *string `json:"thinking"`
}

// listQuery is the parsed and validated query string.
type listQuery struct {
	messageGroupID  *int64
	messageGroupIDs []int64
	kind            string
	limit           int
	offset          int
	includeTotal    bool
	identity        *runIdentity
}

// List serves message_traces.py's prompt_lib GET.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	schema, ok := tenantSchema(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	conversationID, ok := pathID(r, "conversationID")
	if !ok {
		writeError(w, http.StatusBadRequest, "conversation id must be a positive integer")
		return
	}
	query, err := parseListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	var reader traceReader = h.pool
	if query.identity != nil {
		if err := query.identity.bindActor(r); err != nil {
			writeError(w, http.StatusForbidden, "trace execution requires its original actor")
			return
		}
		tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read trace steps")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		reader = tx
	}
	var total *int64
	if query.includeTotal {
		counted, err := h.countSteps(ctx, reader, schema, conversationID, query)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read trace steps")
			return
		}
		total = &counted
	}

	rows, err := h.listSteps(ctx, reader, schema, conversationID, query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read trace steps")
		return
	}
	// `total` is null unless asked for — the reference's shape, and a client
	// can tell "not counted" from "counted zero".
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}

type traceReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// listConditions renders the shared WHERE of the list and its count, so the two
// can never disagree about which rows the page is a page OF.
//
// The blank-thinking-step exclusion lives here rather than in the caller for
// the same reason: a count that included markers the listing drops would report
// a page size no client could reach.
func listConditions(schema string, conversationID int64, query listQuery, args *argList) string {
	conditions := []string{
		"message_group.conversation_id = " + args.add(conversationID),
		"(trace.kind <> 'thinking_step' OR trace.has_visible_content)",
	}
	if query.messageGroupID != nil {
		conditions = append(conditions, "trace.message_group_id = "+args.add(*query.messageGroupID))
	}
	if len(query.messageGroupIDs) > 0 {
		conditions = append(conditions, "trace.message_group_id = ANY("+args.add(query.messageGroupIDs)+")")
	}
	if query.kind != "" {
		conditions = append(conditions, "trace.kind = "+args.add(query.kind))
	}
	conditions = append(conditions, runIdentityConditions(schema, query.identity, args))
	return strings.Join(conditions, " AND ")
}

func (h *Handler) countSteps(
	ctx context.Context, reader traceReader, schema string, conversationID int64, query listQuery,
) (int64, error) {
	args := &argList{}
	statement := fmt.Sprintf(`
SELECT count(*)
FROM %s.chat_message_trace_step AS trace
JOIN %s.chat_message_group AS message_group ON message_group.id = trace.message_group_id
WHERE %s`, schema, schema, listConditions(schema, conversationID, query, args))

	var total int64
	if err := reader.QueryRow(ctx, statement, args.values...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (h *Handler) listSteps(
	ctx context.Context, reader traceReader, schema string, conversationID int64, query listQuery,
) ([]listItem, error) {
	args := &argList{}
	where := listConditions(schema, conversationID, query, args)
	// Ordered by (started_at, id): render order is derived from timestamps,
	// and NULLS LAST keeps a step that never started from jumping to the top of
	// the pin strip. `id` breaks ties so paging is stable.
	statement := fmt.Sprintf(`
SELECT trace.id, trace.message_group_id, trace.kind, trace.tool_name,
       trace.parent_agent_name, trace.parent_agent_call_id,
       trace.started_at, trace.finished_at, trace.is_error,
       trace.step_type, trace.model_name, trace.finish_reason, trace.attrs
FROM %s.chat_message_trace_step AS trace
JOIN %s.chat_message_group AS message_group ON message_group.id = trace.message_group_id
WHERE %s
ORDER BY trace.started_at ASC NULLS LAST, trace.id ASC
LIMIT %s OFFSET %s`,
		schema, schema, where, args.add(query.limit), args.add(query.offset))

	pgRows, err := reader.Query(ctx, statement, args.values...)
	if err != nil {
		return nil, err
	}
	defer pgRows.Close()

	// Never nil: an empty listing marshals as [] rather than null.
	items := []listItem{}
	for pgRows.Next() {
		var item listItem
		var attrs []byte
		if err := pgRows.Scan(
			&item.ID, &item.MessageGroupID, &item.Kind, &item.ToolName,
			&item.ParentAgentName, &item.ParentAgentCallID,
			&item.StartedAt, &item.FinishedAt, &item.IsError,
			&item.StepType, &item.ModelName, &item.FinishReason, &attrs,
		); err != nil {
			return nil, err
		}
		item.Attrs = rawJSON(attrs)
		items = append(items, item)
	}
	return items, pgRows.Err()
}

/* ── detail ────────────────────────────────────────────────────────────── */

// Get serves message_trace.py's prompt_lib GET.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	schema, ok := tenantSchema(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "project id must be a positive integer")
		return
	}
	stepID, ok := pathID(r, "stepID")
	if !ok {
		writeError(w, http.StatusBadRequest, "step id must be a positive integer")
		return
	}
	// Required, and the reference's own 400: without it the step id alone would
	// address every trace row in the project.
	messageGroupID, err := positiveInt(r.URL.Query().Get("message_group_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "message_group_id is required")
		return
	}

	identity, err := parseRunIdentity(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if identity != nil {
		if err := identity.bindActor(r); err != nil {
			writeError(w, http.StatusForbidden, "trace execution requires its original actor")
			return
		}
	}
	args := &argList{values: []any{stepID, messageGroupID}}
	where := runIdentityConditions(schema, identity, args)
	statement := fmt.Sprintf(`
SELECT trace.id, trace.message_group_id, trace.kind, trace.tool_name,
       trace.parent_agent_name, trace.parent_agent_call_id,
       trace.started_at, trace.finished_at, trace.is_error,
       trace.step_type, trace.model_name, trace.finish_reason, trace.attrs,
       trace.tool_inputs, trace.tool_output, trace.text, trace.thinking
FROM %s.chat_message_trace_step AS trace
JOIN %s.chat_message_group AS message_group ON message_group.id=trace.message_group_id
WHERE trace.id = $1 AND trace.message_group_id = $2 AND %s`, schema, schema, where)

	var item detailItem
	var attrs, toolInputs []byte
	err = h.pool.QueryRow(r.Context(), statement, args.values...).Scan(
		&item.ID, &item.MessageGroupID, &item.Kind, &item.ToolName,
		&item.ParentAgentName, &item.ParentAgentCallID,
		&item.StartedAt, &item.FinishedAt, &item.IsError,
		&item.StepType, &item.ModelName, &item.FinishReason, &attrs,
		&toolInputs, &item.ToolOutput, &item.Text, &item.Thinking,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no such trace step with id %d", stepID))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to read trace step")
		return
	}
	item.Attrs = rawJSON(attrs)
	item.ToolInputs = rawJSON(toolInputs)
	writeJSON(w, http.StatusOK, item)
}

/* ── parsing ───────────────────────────────────────────────────────────── */

func parseListQuery(r *http.Request) (listQuery, error) {
	query := r.URL.Query()

	kind := query.Get("kind")
	if kind != "" {
		if _, known := stepKinds[kind]; !known {
			return listQuery{}, fmt.Errorf("unsupported trace-step kind: %s", kind)
		}
	}

	groupIDs, err := parseMessageGroupIDs(query.Get("message_group_ids"))
	if err != nil {
		return listQuery{}, err
	}

	parsed := listQuery{
		messageGroupIDs: groupIDs,
		kind:            kind,
		limit:           defaultLimit,
		includeTotal:    strings.EqualFold(query.Get("include_total"), "true"),
	}
	// A NARROWING filter that cannot be parsed is rejected, not dropped.
	//
	// The reference drops it — Flask's `type=int` yields None on a bad value and
	// the query simply goes unfiltered — and this is a deliberate divergence
	// from that. Dropping it fails OPEN: `?message_group_id=undefined`, the
	// shape a client produces when it builds the id from an uninitialised
	// variable, would silently return every step in the conversation instead of
	// the one message's, and the caller would render pins for messages the user
	// never expanded. The sibling detail read already 400s on exactly this
	// input, so dropping it here also made one parameter mean two things.
	//
	// Absent is still absent: no parameter means no filter, which is the
	// documented way to ask for the whole conversation.
	if raw := query.Get("message_group_id"); raw != "" {
		single, err := positiveInt(raw)
		if err != nil {
			return listQuery{}, errors.New("message_group_id must be a positive integer")
		}
		parsed.messageGroupID = &single
	}
	// `limit` and `offset` keep the reference's clamping instead, and the
	// difference is not inconsistency: a bad limit falls back to a NARROWER
	// answer (the default page), while a bad filter would have widened one.
	// Clamping cannot show a caller rows they did not ask for.
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > 0 {
		parsed.limit = min(limit, maxLimit)
	}
	if offset, err := strconv.Atoi(query.Get("offset")); err == nil && offset > 0 {
		parsed.offset = offset
	}
	identity, err := parseRunIdentity(r)
	if err != nil {
		return listQuery{}, err
	}
	if identity != nil && parsed.messageGroupID == nil {
		return listQuery{}, errors.New("message_group_id is required with execution identity")
	}
	parsed.identity = identity
	return parsed, nil
}

// parseMessageGroupIDs reads the comma-separated loaded-group list, rejecting a
// non-integer member and a list longer than the cap. Duplicates are collapsed:
// the client sends the ids of the pages it has loaded and may repeat one across
// two "load more" rounds.
func parseMessageGroupIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[int64]struct{}{}
	ids := []int64{}
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		value, err := positiveInt(trimmed)
		if err != nil {
			return nil, errors.New("message_group_ids must contain 1-200 positive integers")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}
	if len(ids) > maxMessageGroupID {
		return nil, errors.New("message_group_ids must contain 1-200 positive integers")
	}
	return ids, nil
}

func positiveInt(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, err
	}
	if value < 1 {
		return 0, errors.New("must be positive")
	}
	return value, nil
}

func pathID(r *http.Request, param string) (int64, bool) {
	value, err := positiveInt(chi.URLParam(r, param))
	if err != nil {
		return 0, false
	}
	return value, true
}

// tenantSchema builds the quoted `p_<projectID>` identifier the reads run in.
// The project id is parsed as an integer first and quoted after, so nothing a
// caller can type reaches SQL as an identifier.
func tenantSchema(r *http.Request) (string, bool) {
	projectID, ok := pathID(r, "projectID")
	if !ok {
		return "", false
	}
	return pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10)}.Sanitize(), true
}

// argList accumulates bound parameters and hands out their `$n` placeholders,
// so a condition can never disagree with its argument's position. Same shape as
// internal/api/v2/eliteacore/audit_query.go's.
type argList struct {
	values []any
}

func (a *argList) add(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// rawJSON passes a jsonb column through as JSON rather than as a quoted string.
// A NULL column becomes a JSON null, which is what the reference's
// `Optional[dict] = None` marshals to.
func rawJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}

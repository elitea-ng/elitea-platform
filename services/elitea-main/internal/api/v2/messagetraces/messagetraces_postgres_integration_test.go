package messagetraces_test

// Issue 253 acceptance for the two chat trace reads.
//
// The rows are planted in a real tenant schema and every case asserts WHICH
// rows came back and in WHAT ORDER, because a 200 with the wrong rows is the
// failure this repo keeps shipping (#128). In particular:
//
//   - the listing is checked to EXCLUDE a blank thinking step and a step from
//     another conversation, and to order by (started_at, id) with a null
//     timestamp last — an implementation that returned the table in insertion
//     order passes a status check and fails here;
//   - the listing is checked to omit the heavy columns entirely, which is the
//     reason the endpoint is split in two;
//   - the detail read is checked to 404 when the step id is real but belongs to
//     a DIFFERENT message group, which is the whole of its access scoping;
//   - `total` is checked to be null unless asked for, so "not counted" and
//     "counted zero" stay distinguishable.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/messagetraces"
)

const (
	traceProjectID = 41
	// The two conversations: one under test, one that must never leak into it.
	traceConversationID = 1
	otherConversationID = 2
)

var traceBase = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

/* ── harness ───────────────────────────────────────────────────────────── */

func tracesRouter(pool *pgxpool.Pool) chi.Router {
	traces := handler.NewHandler(pool)
	router := chi.NewRouter()
	router.Get("/message_traces/prompt_lib/{projectID}/{conversationID}", traces.List)
	router.Get("/message_trace/prompt_lib/{projectID}/{stepID}", traces.Get)
	return router
}

func tracesDo(t *testing.T, router chi.Router, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decodeTraces(t *testing.T, recorder *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, want, recorder.Body.String())
	}
	decoder := json.NewDecoder(recorder.Body)
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload
}

// stepIDs is the assertion that carries the ordering: a slice, not a set.
func stepIDs(t *testing.T, payload map[string]any) []string {
	t.Helper()
	rows, ok := payload["rows"].([]any)
	if !ok {
		t.Fatalf("rows missing from %v", payload)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, fmt.Sprint(row.(map[string]any)["id"]))
	}
	return ids
}

func newTracesPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_traces_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

// seedTenantSchema builds the chat tables in `p_41` with the SAME column set
// internal/infra/db/repos/agent_trace.go writes and the deployed tenant schema
// carries (internal/db/schema/agent_chat_baseline.sql). Only the tables these
// two reads touch are created; the projection's other tables are irrelevant
// here and their absence cannot mask a defect in a query that never names them.
func seedTenantSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	schema := pgx.Identifier{fmt.Sprintf("p_%d", traceProjectID)}.Sanitize()
	_, err := pool.Exec(context.Background(), fmt.Sprintf(`
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.chat_conversations (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, name VARCHAR,
    is_private BOOLEAN NOT NULL DEFAULT TRUE, author_id INTEGER NOT NULL,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb, source VARCHAR NOT NULL DEFAULT 'elitea',
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP
);
CREATE TABLE %[1]s.chat_participants (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, entity_name VARCHAR NOT NULL,
    entity_meta JSONB NOT NULL DEFAULT '{}'::jsonb, meta JSON NOT NULL DEFAULT '{}'::json
);
CREATE TABLE %[1]s.chat_message_group (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE,
    author_participant_id INTEGER NOT NULL REFERENCES %[1]s.chat_participants(id),
    conversation_id INTEGER NOT NULL REFERENCES %[1]s.chat_conversations(id),
    sent_to_id INTEGER, reply_to_id INTEGER REFERENCES %[1]s.chat_message_group(id),
    meta JSONB NOT NULL DEFAULT '{}'::jsonb, is_streaming BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP, task_id VARCHAR(64)
);
CREATE TABLE %[1]s.chat_message_trace_step (
    id BIGSERIAL PRIMARY KEY,
    message_group_id INTEGER NOT NULL REFERENCES %[1]s.chat_message_group(id) ON DELETE CASCADE,
    kind TEXT NOT NULL, run_id TEXT, parent_agent_name TEXT, parent_agent_call_id TEXT,
    started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, is_error BOOLEAN NOT NULL DEFAULT FALSE,
    has_visible_content BOOLEAN NOT NULL DEFAULT TRUE, tool_name TEXT, tool_inputs JSONB,
    tool_output TEXT, finish_reason TEXT, step_type TEXT, text TEXT, thinking TEXT,
    model_name TEXT, attrs JSONB
);
INSERT INTO %[1]s.chat_participants (id, uuid, entity_name)
VALUES (1, '30000000-0000-4000-8000-000000000001', 'agent');
INSERT INTO %[1]s.chat_conversations (id, uuid, author_id, created_at)
VALUES (1, '10000000-0000-4000-8000-000000000001', 7, now() AT TIME ZONE 'UTC'),
       (2, '10000000-0000-4000-8000-000000000002', 7, now() AT TIME ZONE 'UTC');
INSERT INTO %[1]s.chat_message_group (id, uuid, author_participant_id, conversation_id, created_at)
VALUES (11, '20000000-0000-4000-8000-000000000011', 1, 1, now() AT TIME ZONE 'UTC'),
       (12, '20000000-0000-4000-8000-000000000012', 1, 1, now() AT TIME ZONE 'UTC'),
       (21, '20000000-0000-4000-8000-000000000021', 1, 2, now() AT TIME ZONE 'UTC');`, schema))
	if err != nil {
		t.Fatalf("seed tenant schema: %v", err)
	}
}

type seededStep struct {
	id             int64
	messageGroupID int64
	kind           string
	visible        bool
	startedAt      *time.Time
	toolName       string
	toolOutput     string
	thinking       string
	attrs          string
}

func plantStep(t *testing.T, pool *pgxpool.Pool, step seededStep) {
	t.Helper()
	schema := pgx.Identifier{fmt.Sprintf("p_%d", traceProjectID)}.Sanitize()
	_, err := pool.Exec(context.Background(), fmt.Sprintf(`
INSERT INTO %s.chat_message_trace_step
    (id, message_group_id, kind, has_visible_content, started_at,
     tool_name, tool_inputs, tool_output, thinking, attrs)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10::jsonb)`, schema),
		step.id, step.messageGroupID, step.kind, step.visible, step.startedAt,
		nullable(step.toolName), `{"query":"select 1"}`, nullable(step.toolOutput),
		nullable(step.thinking), nullable(step.attrs))
	if err != nil {
		t.Fatalf("plant step %d: %v", step.id, err)
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func at(offset time.Duration) *time.Time {
	moment := traceBase.Add(offset)
	return &moment
}

// newTracesEnvironment plants the fixture the ordering and exclusion cases all
// read. Insertion order is deliberately NOT chronological order: step 101 is
// inserted first but starts last, so a query that forgot its ORDER BY returns a
// different sequence than the one asserted.
func newTracesEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newTracesPool(t)
	seedTenantSchema(t, pool)

	plantStep(t, pool, seededStep{
		id: 101, messageGroupID: 11, kind: "tool_call", visible: true,
		startedAt: at(30 * time.Second), toolName: "search",
		toolOutput: "forty-two", attrs: `{"icon":"search"}`,
	})
	plantStep(t, pool, seededStep{
		id: 102, messageGroupID: 11, kind: "thinking_step", visible: true,
		startedAt: at(10 * time.Second), thinking: "considering the options",
	})
	// A transition marker: an action with no text. The client draws a pin for
	// every row it receives, so this one must never be handed to it.
	plantStep(t, pool, seededStep{
		id: 103, messageGroupID: 11, kind: "thinking_step", visible: false,
		startedAt: at(20 * time.Second),
	})
	// A step that never started: ordered LAST, not first.
	plantStep(t, pool, seededStep{
		id: 104, messageGroupID: 12, kind: "tool_call", visible: true,
		toolName: "pending",
	})
	// Another conversation entirely.
	plantStep(t, pool, seededStep{
		id: 201, messageGroupID: 21, kind: "tool_call", visible: true,
		startedAt: at(5 * time.Second), toolName: "elsewhere",
	})
	return pool, tracesRouter(pool)
}

/* ── listing: which rows, in which order ───────────────────────────────── */

func TestTraceListingOrdersByStartAndExcludesWhatTheClientMustNotDraw(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router,
		fmt.Sprintf("/message_traces/prompt_lib/%d/%d", traceProjectID, traceConversationID)),
		http.StatusOK)

	// 102 (t+10s), 101 (t+30s), 104 (never started, last). 103 is the blank
	// marker and 201 belongs to the other conversation; neither appears.
	got := stepIDs(t, body)
	want := []string{"102", "101", "104"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v (chronological, nulls last, blanks and "+
			"other conversations excluded)", got, want)
	}

	// Not asked for, so not counted — and null rather than 0.
	if total, present := body["total"]; !present || total != nil {
		t.Fatalf("total = %#v, want present-and-null when include_total is absent", total)
	}
}

func TestTraceListingCountsOnlyWhenAsked(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?include_total=true&limit=1",
		traceProjectID, traceConversationID)), http.StatusOK)

	// The page is one row; the count is of the whole filtered set, and it
	// excludes the blank marker exactly as the listing does — a count that
	// disagreed would report a page size no client could reach.
	if fmt.Sprint(body["total"]) != "3" {
		t.Fatalf("total = %v, want 3", body["total"])
	}
	if ids := stepIDs(t, body); len(ids) != 1 || ids[0] != "102" {
		t.Fatalf("rows = %v, want the first page only", ids)
	}
}

/* ── listing: the light projection ─────────────────────────────────────── */

func TestTraceListingCarriesNoHeavyFields(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router,
		fmt.Sprintf("/message_traces/prompt_lib/%d/%d", traceProjectID, traceConversationID)),
		http.StatusOK)

	row := body["rows"].([]any)[1].(map[string]any) // step 101, the tool call
	for _, heavy := range []string{"tool_inputs", "tool_output", "text", "thinking"} {
		if _, present := row[heavy]; present {
			t.Fatalf("%s is present in the listing: this is the projection that "+
				"renders every chip in a conversation, and these columns are TOASTed", heavy)
		}
	}
	// The bounded display sidecar IS here, as raw JSON rather than a quoted
	// string: it is what lets a reloaded chip render without a detail fetch.
	attrs, ok := row["attrs"].(map[string]any)
	if !ok || attrs["icon"] != "search" {
		t.Fatalf("attrs = %#v, want the jsonb object passed through", row["attrs"])
	}
}

/* ── listing: filters ──────────────────────────────────────────────────── */

func TestTraceListingFiltersByGroupAndKind(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?message_group_id=11",
		traceProjectID, traceConversationID)), http.StatusOK)
	if got := stepIDs(t, body); fmt.Sprint(got) != fmt.Sprint([]string{"102", "101"}) {
		t.Fatalf("rows = %v, want group 11 only", got)
	}

	body = decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?kind=tool_call",
		traceProjectID, traceConversationID)), http.StatusOK)
	if got := stepIDs(t, body); fmt.Sprint(got) != fmt.Sprint([]string{"101", "104"}) {
		t.Fatalf("rows = %v, want the tool calls only", got)
	}

	body = decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?message_group_ids=12,12",
		traceProjectID, traceConversationID)), http.StatusOK)
	if got := stepIDs(t, body); fmt.Sprint(got) != fmt.Sprint([]string{"104"}) {
		t.Fatalf("rows = %v, want group 12 only (duplicates collapsed)", got)
	}

	// An unknown kind is a 400, not an empty page that reads as "no tool calls".
	recorder := tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?kind=teapot", traceProjectID, traceConversationID))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d for an unknown kind, want 400", recorder.Code)
	}
}

// A narrowing filter that cannot be parsed must not be dropped: dropping it
// fails OPEN and hands back the whole conversation.
//
// `undefined` is the literal a client sends when it builds the id from an
// uninitialised variable, and it is the case that makes this more than
// pedantry — the caller believes it asked for one message's steps and renders
// every pin in the conversation. An absent parameter still means "no filter".
func TestTraceListingRejectsAMalformedGroupFilterRatherThanWidening(t *testing.T) {
	_, router := newTracesEnvironment(t)

	for _, raw := range []string{"undefined", "abc", "0", "-1", "1.5"} {
		target := fmt.Sprintf("/message_traces/prompt_lib/%d/%d?message_group_id=%s",
			traceProjectID, traceConversationID, raw)
		recorder := tracesDo(t, router, target)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d for message_group_id=%s, want 400 — a dropped filter "+
				"returns the whole conversation, which is the opposite of what was asked",
				recorder.Code, raw)
		}
	}

	// The absent case is untouched: no parameter is still the documented way to
	// ask for every step in the conversation.
	body := decodeTraces(t, tracesDo(t, router,
		fmt.Sprintf("/message_traces/prompt_lib/%d/%d", traceProjectID, traceConversationID)),
		http.StatusOK)
	if got := stepIDs(t, body); len(got) != 3 {
		t.Fatalf("rows = %v, want all three with no filter", got)
	}
}

// A message group from ANOTHER conversation, named explicitly, still yields
// nothing: the conversation join is the scope, and the group filter narrows
// within it rather than reaching outside it.
func TestTraceListingCannotReachAnotherConversationsGroup(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_traces/prompt_lib/%d/%d?message_group_id=21",
		traceProjectID, traceConversationID)), http.StatusOK)
	if got := stepIDs(t, body); len(got) != 0 {
		t.Fatalf("rows = %v, want none: group 21 belongs to conversation 2", got)
	}
}

/* ── detail ────────────────────────────────────────────────────────────── */

func TestTraceDetailReturnsTheHeavyFields(t *testing.T) {
	_, router := newTracesEnvironment(t)

	body := decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_trace/prompt_lib/%d/101?message_group_id=11", traceProjectID)),
		http.StatusOK)

	if fmt.Sprint(body["id"]) != "101" || body["tool_name"] != "search" {
		t.Fatalf("wrong step returned: %v", body)
	}
	if body["tool_output"] != "forty-two" {
		t.Fatalf("tool_output = %v, want the heavy column", body["tool_output"])
	}
	inputs, ok := body["tool_inputs"].(map[string]any)
	if !ok || inputs["query"] != "select 1" {
		t.Fatalf("tool_inputs = %#v, want the jsonb object passed through", body["tool_inputs"])
	}
}

// The pairing IS the access control: a real step id with the wrong owning group
// must not answer, or a numeric id would address every trace in the project.
func TestTraceDetailRefusesAStepFromAnotherMessageGroup(t *testing.T) {
	_, router := newTracesEnvironment(t)

	recorder := tracesDo(t, router, fmt.Sprintf(
		"/message_trace/prompt_lib/%d/101?message_group_id=21", traceProjectID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: step 101 does not belong to group 21", recorder.Code)
	}

	// And the same id with its own group still works, so the 404 above was the
	// pairing and not a broken read.
	decodeTraces(t, tracesDo(t, router, fmt.Sprintf(
		"/message_trace/prompt_lib/%d/101?message_group_id=11", traceProjectID)),
		http.StatusOK)
}

func TestTraceDetailRequiresTheOwningGroup(t *testing.T) {
	_, router := newTracesEnvironment(t)

	for _, target := range []string{
		fmt.Sprintf("/message_trace/prompt_lib/%d/101", traceProjectID),
		fmt.Sprintf("/message_trace/prompt_lib/%d/101?message_group_id=0", traceProjectID),
		fmt.Sprintf("/message_trace/prompt_lib/%d/101?message_group_id=abc", traceProjectID),
	} {
		if recorder := tracesDo(t, router, target); recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d for %q, want 400", recorder.Code, target)
		}
	}
}

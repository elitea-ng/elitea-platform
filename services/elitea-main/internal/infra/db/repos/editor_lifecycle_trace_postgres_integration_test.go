package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/messagetraces"
	agentapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (f editorLifecycleFixture) projectTrace(t *testing.T, request agentapp.SubmitRequest, outcome executionapp.AdmissionOutcome, generation, label string) {
	t.Helper()
	// Only the production trace projector is invoked; this is not a worker-output receipt.
	raw, err := json.Marshal(map[string]any{"type": "partial_message", "stream_id": request.ClientStreamID, "message_id": request.ClientMessageID, "execution_generation": generation, "sio_event": "chat_predict", "response_metadata": map[string]any{"thinking_steps": []any{map[string]any{"tool_run_id": label, "text": label, "timestamp_start": time.Now().UTC().Format(time.RFC3339Nano)}}}})
	if err != nil {
		t.Fatal(err)
	}
	projects, err := newPostgresProjectStore(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	projector := &postgresCurrentAgentTraceProjector{}
	frame := outputapp.NodeEventFrame{Fence: runtimedomain.Fence{ExecutionID: outcome.ExecutionID, Generation: 1}, BrowserData: raw, OccurredAt: time.Now()}
	if err := projects.WithinProjectTx(t.Context(), 1, pgx.TxOptions{}, func(tx sqlExecutor) error { return projector.projectAgentTraceDelta(t.Context(), tx, 1, frame) }); err != nil {
		t.Fatal(err)
	}
}

func editorTraceRequest(ctx context.Context, f editorLifecycleFixture, c conversations.Conversation, actor, target string, detail bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/fixture"+target, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "1")
	if detail {
		route.URLParams.Add("stepID", c.ID)
	} else {
		route.URLParams.Add("conversationID", c.ID)
	}
	request = request.WithContext(context.WithValue(editorLifecycleContext(ctx, actor), chi.RouteCtxKey, route))
	response := httptest.NewRecorder()
	handler := messagetraces.NewHandler(f.pool)
	if detail {
		handler.Get(response, request)
	} else {
		handler.List(response, request)
	}
	return response
}

func editorTraceFence(run conversations.EditorTestRun) string {
	q := url.Values{"message_group_id": {strconv.FormatInt(run.ResponseMessageGroupID, 10)}, "execution_id": {run.ExecutionID}, "execution_generation": {run.ExecutionGeneration}, "response_message_id": {run.ResponseMessageID}, "include_total": {"true"}}
	return "?" + q.Encode()
}

func editorTraceRows(t *testing.T, response *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("trace status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Rows
}

func TestEditorLifecyclePostgresTraceReceiptIdentity(t *testing.T) {
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "99")
	}
	pool := editorLifecyclePostgresPool(t)
	f := newEditorLifecycleFixture(t, pool)
	c := f.create(t, f.first.ID)
	service := editorLifecycleAdmissions(t, pool)
	request := f.submitRequest(t, c)
	outcome, err := service.Submit(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	read := func() conversations.EditorTestRunsPage {
		page, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	original := read().Rows[0]
	if original.ResponseMessageGroupID <= 0 || original.TraceAvailable || !original.AdmittedAt.Equal(outcome.AdmittedAt) || original.SettledAt != nil {
		t.Fatalf("incorrect original receipt %+v", original)
	}
	f.projectTrace(t, request, outcome, request.CurrentTurn.QuestionID, "original-trace")
	page := read()
	original = page.Rows[0]
	if !original.TraceAvailable {
		t.Fatal("production persisted trace unavailable")
	}
	before := editorLifecycleSnapshot(t, pool)
	rows := editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(original), false))
	if len(rows) != 1 || rows[0]["message_group_id"] != float64(original.ResponseMessageGroupID) {
		t.Fatal("original trace group changed", rows)
	}
	stepID := fmt.Sprint(int64(rows[0]["id"].(float64)))
	detailConversation := c
	detailConversation.ID = stepID
	detail := editorTraceRequest(t.Context(), f, detailConversation, "1", editorTraceFence(original), true)
	if detail.Code != 200 || !containsEditorTrace(detail, "original-trace") {
		t.Fatal("original detail unavailable", detail.Body.String())
	}
	if after := editorLifecycleSnapshot(t, pool); after != before {
		t.Fatal("trace reads created durable effects")
	}
	foreign := editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "999", editorTraceFence(original), false))
	if len(foreign) != 0 {
		t.Fatal("foreign actor received trace")
	}
	wrong := original
	wrong.ExecutionGeneration = uuid.NewString()
	if len(editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(wrong), false))) != 0 {
		t.Fatal("wrong generation received trace")
	}
	wrong = original
	wrong.ExecutionID = uuid.NewString()
	if len(editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(wrong), false))) != 0 {
		t.Fatal("wrong execution received trace")
	}
	wrong = original
	wrong.ResponseMessageID = uuid.NewString()
	if len(editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(wrong), false))) != 0 {
		t.Fatal("wrong response received trace")
	}

	// Compete real regeneration admission with old fenced list/detail reads.
	f.persistTerminal(t, request, outcome, false)
	regenerated := request
	generation := uuid.NewString()
	regenerated.IdempotencyKey = generation
	regenerated.CurrentTurn = nil
	turn := request.CurrentTurn
	regenerated.CurrentRegenerateTurn = &agentapp.CurrentRegenerateTurn{ProjectID: turn.ProjectID, ActorUserID: turn.ActorUserID, ConversationUUID: turn.ConversationUUID, TargetParticipantID: turn.TargetParticipantID, Kind: agentapp.CurrentRegenerationApplication, ApplicationID: turn.ApplicationID, ApplicationVersionID: turn.ApplicationVersionID, QuestionID: turn.QuestionID, ResponseMessageID: turn.ResponseMessageID, ExecutionGeneration: generation}
	regenerated.Input = proto.Clone(request.Input).(*runtimev1.AgentExecutionInputV1)
	regenerated.Input.ExecutionGeneration = proto.String(generation)
	var reads sync.WaitGroup
	reads.Add(1)
	start := make(chan struct{})
	failures := make(chan string, 128)
	go func() {
		defer reads.Done()
		close(start)
		for i := 0; i < 40; i++ {
			response := editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(original), false)
			if response.Code != 200 {
				failures <- fmt.Sprintf("concurrent list status=%d", response.Code)
				continue
			}
			var listing struct {
				Rows []map[string]any `json:"rows"`
			}
			if json.Unmarshal(response.Body.Bytes(), &listing) != nil {
				failures <- "invalid concurrent listing"
			}
			if len(listing.Rows) > 1 {
				failures <- "old fence borrowed additional trace rows"
			}
			for _, row := range listing.Rows {
				if row["id"] != rows[0]["id"] || row["message_group_id"] != rows[0]["message_group_id"] {
					failures <- "old fence borrowed another trace identity"
				}
			}
			response = editorTraceRequest(t.Context(), f, detailConversation, "1", editorTraceFence(original), true)
			if response.Code != 404 && (response.Code != 200 || !containsEditorTrace(response, "original-trace")) {
				failures <- "old detail fence borrowed new trace"
			}
		}
	}()
	<-start
	replacement, err := service.Submit(t.Context(), regenerated)
	if err != nil {
		t.Fatal("real regeneration admission", err)
	}
	f.projectTrace(t, regenerated, replacement, generation, "new-trace")
	reads.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	page = read()
	if len(page.Rows) != 2 {
		t.Fatalf("regeneration receipts=%d", len(page.Rows))
	}
	var latest, older conversations.EditorTestRun
	for _, run := range page.Rows {
		if run.ExecutionID == outcome.ExecutionID {
			older = run
		} else {
			latest = run
		}
	}
	if older.TraceAvailable || older.CanControl || older.Phase != "TERMINAL" || latest.ExecutionID != replacement.ExecutionID || latest.ExecutionGeneration != generation || !latest.TraceAvailable || latest.ResponseMessageGroupID != original.ResponseMessageGroupID {
		t.Fatalf("supersession fence invalid: older=%+v latest=%+v", older, latest)
	}
	if len(editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(original), false))) != 0 {
		t.Fatal("superseded trace list borrowed newer response")
	}
	if response := editorTraceRequest(t.Context(), f, detailConversation, "1", editorTraceFence(original), true); response.Code != 404 {
		t.Fatal("superseded trace detail did not refuse")
	}
	latestRows := editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(latest), false))
	if len(latestRows) != 1 || latestRows[0]["id"] == rows[0]["id"] {
		t.Fatal("current receipt trace unavailable")
	}
	t.Log("production admission + trace projection + competing regeneration: original/superseded/cross-actor identities fenced; list/detail create zero effects")
}

func containsEditorTrace(response *httptest.ResponseRecorder, label string) bool {
	var item struct {
		Text string `json:"text"`
	}
	return json.Unmarshal(response.Body.Bytes(), &item) == nil && item.Text == label
}

func TestEditorLifecyclePostgresTracePagination(t *testing.T) {
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "99")
	}
	pool := editorLifecyclePostgresPool(t)
	f := newEditorLifecycleFixture(t, pool)
	c := f.create(t, f.first.ID)
	service := editorLifecycleAdmissions(t, pool)
	type receipt struct {
		request agentapp.SubmitRequest
		outcome executionapp.AdmissionOutcome
		group   int64
	}
	receipts := map[string]receipt{}
	for i := 0; i < 57; i++ {
		request := f.submitRequest(t, c)
		outcome, err := service.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		var group int64
		if err := pool.QueryRow(t.Context(), "SELECT id FROM p_1.chat_message_group WHERE uuid=$1", request.ClientMessageID).Scan(&group); err != nil {
			t.Fatal(err)
		}
		receipts[outcome.ExecutionID] = receipt{request, outcome, group}
		if i == 0 || i == 56 {
			f.projectTrace(t, request, outcome, request.CurrentTurn.QuestionID, fmt.Sprintf("paged-trace-%d", i))
		}
		f.persistTerminal(t, request, outcome, false)
	}
	before := editorLifecycleSnapshot(t, pool)
	seen := map[string]bool{}
	available := 0
	for _, offset := range []int{0, 50} {
		page, err := f.repo.EditorTestRuns(editorLifecycleContext(t.Context(), "1"), "1", c.ID, 50, offset)
		if err != nil {
			t.Fatal(err)
		}
		want := 50
		if offset == 50 {
			want = 7
		}
		if len(page.Rows) != want || page.HasMore != (offset == 0) {
			t.Fatalf("offset %d page=%+v", offset, page)
		}
		for _, run := range page.Rows {
			receipt, ok := receipts[run.ExecutionID]
			if !ok || seen[run.ExecutionID] {
				t.Fatal("unknown/duplicate receipt")
			}
			seen[run.ExecutionID] = true
			if run.ResponseMessageGroupID != receipt.group || run.ResponseMessageID != receipt.request.ClientMessageID || run.ExecutionGeneration != receipt.request.CurrentTurn.QuestionID || !run.AdmittedAt.Equal(receipt.outcome.AdmittedAt) || run.SettledAt != nil {
				t.Fatal("paged original identity/timing changed")
			}
			if run.TraceAvailable {
				available++
				if len(editorTraceRows(t, editorTraceRequest(t.Context(), f, c, "1", editorTraceFence(run), false))) != 1 {
					t.Fatal("paged trace not bound")
				}
			}
		}
	}
	if len(seen) != 57 || available != 2 {
		t.Fatalf("seen=%d available=%d", len(seen), available)
	}
	if after := editorLifecycleSnapshot(t, pool); after != before {
		t.Fatal("pagination and trace reads wrote effects")
	}
	t.Log("57 real admissions: original numeric response identities and authoritative admission timestamps preserved across 50+7 pages; two production traces selected exactly; zero read effects")
}

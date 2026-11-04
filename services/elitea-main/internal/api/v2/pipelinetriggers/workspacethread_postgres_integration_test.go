package pipelinetriggers_test

// Schedules and webhook triggers that target a handed-off workspace thread
// (ADR-0045 decision 9, tenant 0159). The settings, the stored columns and
// the occurrence's outcomes run against PostgreSQL; the thread seam is a
// double, because this database has no elitea_runtime schema. The seam's
// own SQL, the CLOUD_IDLE fence and the start are tested in repos
// (workspace_thread_target_postgres_integration_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/handoffcontinue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// fakeThreadRuns is the handed-off thread seam.
type fakeThreadRuns struct {
	mu        sync.Mutex
	handedOff bool
	active    bool
	activeIDs []string
	err       error
	turns     []handoffcontinue.ThreadTurn
}

func (f *fakeThreadRuns) ThreadHandedOff(context.Context, int64, string) (bool, error) {
	return f.handedOff, nil
}

func (f *fakeThreadRuns) RunActive(_ context.Context, _ int64, executionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeIDs = append(f.activeIDs, executionID)
	return f.active, nil
}

func (f *fakeThreadRuns) StartThreadTurn(
	_ context.Context, turn handoffcontinue.ThreadTurn,
) (handoffcontinue.ThreadTurnStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return handoffcontinue.ThreadTurnStarted{}, f.err
	}
	f.turns = append(f.turns, turn)
	return handoffcontinue.ThreadTurnStarted{
		ExecutionID: fmt.Sprintf("thread-exec-%d", len(f.turns)), QuestionID: turn.QuestionID,
		ResponseMessageID: "22222222-2222-4222-8222-222222222222", ConversationUUID: turn.ConversationUUID,
	}, nil
}

func (f *fakeThreadRuns) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.turns)
}

func (f *fakeThreadRuns) set(err error, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.active = err, active
}

// newThreadHarness is newHarness with the thread seam.
func newThreadHarness(t *testing.T) (*harness, *fakeThreadRuns) {
	t.Helper()
	h := newHarness(t)
	threads := &fakeThreadRuns{handedOff: true}
	h.handler = pipelinetriggers.NewPlatformHandler(
		h.pool, h.start, h.vault,
		fixedPermissions{userID: ownerUserID, projectID: homeProject, permissions: []string{pipelinetriggers.RunPermission}},
		h.recorder, nil, nil, nil,
		pipelinetriggers.WithThreadRuns(threads),
	)
	h.router = mountRoutes(h.handler)
	return h, threads
}

// seedThread writes a conversation the user participant `user` belongs to,
// and answers its uuid.
func seedThread(t *testing.T, h *harness, user int64) string {
	t.Helper()
	ctx := context.Background()
	var conversationID int64
	var conversationUUID string
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source, created_at)
VALUES (gen_random_uuid(), 'Local work', TRUE, $1, '{}'::jsonb, 'elitea', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, homeSchema), user).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	var participantID int64
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'user', jsonb_build_object('id', $1::bigint), '{}'::json)
RETURNING id`, homeSchema), user).Scan(&participantID); err != nil {
		t.Fatalf("seed user participant: %v", err)
	}
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, '{}'::jsonb, now() AT TIME ZONE 'UTC')`, homeSchema), conversationID, participantID); err != nil {
		t.Fatalf("map user participant: %v", err)
	}
	return conversationUUID
}

func scheduleRoute(versionID int64) string {
	return fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID)
}

func triggerRoute(versionID int64) string {
	return fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID)
}

func threadBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAThreadScheduleSaveChecksTheTarget(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	stranger := seedThread(t, h, ownerUserID+1)
	valid := map[string]any{
		"cron": "0 7 * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.",
	}
	with := func(changes map[string]any) map[string]any {
		body := map[string]any{}
		for key, value := range valid {
			body[key] = value
		}
		for key, value := range changes {
			if value == nil {
				delete(body, key)
				continue
			}
			body[key] = value
		}
		return body
	}
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"no prompt", with(map[string]any{"prompt": nil}), http.StatusBadRequest, ""},
		{"blank prompt", with(map[string]any{"prompt": "   "}), http.StatusBadRequest, ""},
		{"long prompt", with(map[string]any{"prompt": strings.Repeat("x", 32769)}), http.StatusBadRequest, ""},
		{"bad uuid", with(map[string]any{"conversation_uuid": "nope"}), http.StatusBadRequest, ""},
		{"thread fields without the kind", map[string]any{"cron": "0 7 * * *", "prompt": "x"}, http.StatusBadRequest, ""},
		{"unknown kind", with(map[string]any{"target_kind": "laptop"}), http.StatusBadRequest, ""},
		{"not the caller's thread", with(map[string]any{"conversation_uuid": stranger}), http.StatusNotFound, ""},
		{"no such thread", with(map[string]any{"conversation_uuid": "0b7e4c1a-2d3f-4a5b-9c6d-7e8f9a0b1c2d"}),
			http.StatusNotFound, ""},
		{"sensitive tools", with(map[string]any{"grant": map[string]any{
			"sensitive_tools": []map[string]any{{"toolkit_name": "github", "tool_name": "merge"}}}}),
			http.StatusUnprocessableEntity, "unattended_grant_refused"},
		{"hook asks", with(map[string]any{"grant": map[string]any{"hook_asks": true}}),
			http.StatusUnprocessableEntity, "unattended_grant_refused"},
		{"remote confirmations", with(map[string]any{"grant": map[string]any{"remote_confirmations": true}}),
			http.StatusUnprocessableEntity, "unattended_grant_refused"},
		{"chain over the maximum", with(map[string]any{"grant": map[string]any{
			"continue_step_limit": true, "chain": map[string]any{"max_continuations": 100000}}}),
			http.StatusUnprocessableEntity, "unattended_grant_refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, tc.body), nil)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, tc.status, response.Body.String())
			}
			if tc.code != "" && decode(t, response)["error"] != tc.code {
				t.Fatalf("body = %s, want code %s", response.Body.String(), tc.code)
			}
		})
	}

	threads.handedOff = false
	response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, valid), nil)
	if response.Code != http.StatusUnprocessableEntity || decode(t, response)["code"] != "not_handed_off" {
		t.Fatalf("a thread never handed off: status = %d, body = %s", response.Code, response.Body.String())
	}
	if view := h.readSchedule(t, versionID); view["configured"] != false {
		t.Fatalf("a refused save stored a schedule: %v", view)
	}

	// Without the seam the target is not available on this deployment.
	plain := newHarness(t)
	plainVersion := seedPipeline(t, plain.pool, homeSchema, "Thread anchor", ownerUserID)
	plainThread := seedThread(t, plain, ownerUserID)
	valid["conversation_uuid"] = plainThread
	if response := plain.do(t, http.MethodPut, scheduleRoute(plainVersion), threadBody(t, valid), nil); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("no seam: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAThreadScheduleStoresAndShowsItsTarget(t *testing.T) {
	h, _ := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "0 7 * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": strings.ToUpper(thread), "prompt": "Run the nightly checks.",
	}), nil)
	// An upper-case uuid is the same thread; it is stored in lower case.
	if response.Code != http.StatusOK || decode(t, response)["conversation_uuid"] != thread {
		t.Fatalf("upper-case uuid: status = %d, body = %s", response.Code, response.Body.String())
	}
	response = h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "0 7 * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.",
		"grant": map[string]any{"continue_step_limit": true, "goal_check": false, "chain": map[string]any{"max_continuations": 3}},
	}), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body = %s", response.Code, response.Body.String())
	}
	view := decode(t, response)
	grant, _ := view["grant"].(map[string]any)
	if view["target_kind"] != "workspace_thread" || view["conversation_uuid"] != thread ||
		view["prompt"] != "Run the nightly checks." || grant["continue_step_limit"] != true ||
		grant["continue_output_limit"] != false || grant["goal_check"] != false {
		t.Fatalf("view = %v", view)
	}

	// A save with no target_kind keeps the target.
	response = h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 8 * * *","active":true}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("resave: status = %d, body = %s", response.Code, response.Body.String())
	}
	if view := decode(t, response); view["target_kind"] != "workspace_thread" || view["prompt"] != "Run the nightly checks." ||
		view["cron"] != "0 8 * * *" {
		t.Fatalf("a save without target_kind changed the target: %v", view)
	}

	// Back to the pipeline: the thread columns are cleared (0159 CHECK).
	response = h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 8 * * *","target_kind":"pipeline"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("to pipeline: status = %d, body = %s", response.Code, response.Body.String())
	}
	if view := decode(t, response); view["target_kind"] != "pipeline" || view["conversation_uuid"] != nil || view["grant"] != nil {
		t.Fatalf("view = %v", view)
	}

	// A thread target's anchor may be an agent version: the version does not run.
	agentVersion := seedPipeline(t, h.pool, homeSchema, "Agent anchor", ownerUserID)
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE %s.application_versions SET agent_type = 'react' WHERE id = $1`, homeSchema), agentVersion); err != nil {
		t.Fatal(err)
	}
	response = h.do(t, http.MethodPut, scheduleRoute(agentVersion), threadBody(t, map[string]any{
		"cron": "0 7 * * *", "target_kind": "workspace_thread", "conversation_uuid": thread, "prompt": "Go.",
	}), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("agent anchor: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAThreadScheduleFiresSkipsAndOverlaps(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "* * * * *", "active": true, "input": "ignored", "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.",
	}), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body = %s", response.Code, response.Body.String())
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
	result, err := h.handler.RunDueSchedules(context.Background(), now)
	if err != nil || result.Dispatched != 1 {
		t.Fatalf("tick = %+v, %v", result, err)
	}
	if h.start.count() != 0 {
		t.Fatal("a thread schedule started a pipeline run")
	}
	turn := threads.turns[0]
	if turn.ConversationUUID != thread || turn.Prompt != "Run the nightly checks." || turn.ActorUserID != ownerUserID ||
		turn.TriggerOrigin != executiondomain.TriggerOriginSchedule || turn.QuestionID == "" || turn.VersionID != 0 {
		t.Fatalf("turn = %+v", turn)
	}
	// The absent grant is the decision 10 default.
	if turn.Grant == nil || !turn.Grant.ContinueStepLimit || !turn.Grant.ContinueOutputLimit || !turn.Grant.GoalCheck ||
		!turn.Grant.OnlyContinues() {
		t.Fatalf("grant = %+v", turn.Grant)
	}
	view := h.readSchedule(t, versionID)
	if view["last_result"] != "dispatched" || view["last_execution_id"] != "thread-exec-1" {
		t.Fatalf("view = %v", view)
	}
	firstRun := view["last_run"]

	// OVERLAP through last_execution_id.
	threads.set(nil, true)
	result, err = h.handler.RunDueSchedules(context.Background(), now.Add(2*time.Minute))
	if err != nil || result.Skipped != 1 || threads.count() != 1 {
		t.Fatalf("overlap tick = %+v, %v", result, err)
	}
	if threads.activeIDs[len(threads.activeIDs)-1] != "thread-exec-1" {
		t.Fatalf("the overlap check read %v", threads.activeIDs)
	}
	if view := h.readSchedule(t, versionID); view["last_result"] != "skipped_overlap" || view["last_run"] != firstRun {
		t.Fatalf("view = %v", view)
	}

	// BUSY: the thread is not CLOUD_IDLE.
	threads.set(fmt.Errorf("wrapped: %w", handoffcontinue.ErrThreadBusy), false)
	result, err = h.handler.RunDueSchedules(context.Background(), now.Add(4*time.Minute))
	if err != nil || result.Skipped != 1 {
		t.Fatalf("busy tick = %+v, %v", result, err)
	}
	view = h.readSchedule(t, versionID)
	if view["last_result"] != "skipped_thread_busy" || view["last_run"] != firstRun ||
		view["last_execution_id"] != "thread-exec-1" {
		t.Fatalf("busy view = %v", view)
	}

	// GONE: the existing missing result.
	threads.set(workspacehandoff.ErrNotFound, false)
	if _, err := h.handler.RunDueSchedules(context.Background(), now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if view := h.readSchedule(t, versionID); view["last_result"] != "skipped_missing_version" {
		t.Fatalf("gone view = %v", view)
	}
}

// mintThreadTrigger creates a trigger with a thread target.
func mintThreadTrigger(t *testing.T, h *harness, versionID int64, thread string, extra map[string]any) (string, string) {
	t.Helper()
	body := map[string]any{"target_kind": "workspace_thread", "conversation_uuid": thread, "prompt": "Triage the alert."}
	for key, value := range extra {
		body[key] = value
	}
	response := h.do(t, http.MethodPost, triggerRoute(versionID), threadBody(t, body), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint: status = %d, body = %s", response.Code, response.Body.String())
	}
	view := decode(t, response)
	if view["target_kind"] != "workspace_thread" || view["conversation_uuid"] != thread || view["prompt"] != "Triage the alert." {
		t.Fatalf("view = %v", view)
	}
	return view["token_id"].(string), view["secret"].(string)
}

func TestAThreadTriggerStartsATurnAndAnswers409WhenBusy(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	tokenID, secret := mintThreadTrigger(t, h, versionID, thread, map[string]any{
		"grant": map[string]any{"continue_step_limit": true, "continue_output_limit": true},
	})
	bearer := map[string]string{"Authorization": "Bearer " + secret}

	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"disk full on db-2"}`, bearer)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body := decode(t, response); body["execution_id"] != "thread-exec-1" || body["conversation_id"] != thread {
		t.Fatalf("body = %v", body)
	}
	turn := threads.turns[0]
	if turn.Prompt != "Triage the alert.\n\ndisk full on db-2" || turn.TriggerOrigin != executiondomain.TriggerOriginWebhook ||
		turn.ActorUserID != ownerUserID || turn.Grant == nil || !turn.Grant.GoalCheck {
		t.Fatalf("turn = %+v", turn)
	}
	if h.start.count() != 0 {
		t.Fatal("a thread trigger started an agent run")
	}
	view := decode(t, h.do(t, http.MethodGet, triggerRoute(versionID), "", nil))
	if view["last_execution_id"] != "thread-exec-1" || view["last_used_at"] == nil {
		t.Fatalf("view = %v", view)
	}

	// No body: the prompt alone.
	if response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", bearer); response.Code != http.StatusAccepted {
		t.Fatalf("no body: status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads.turns[1].Prompt != "Triage the alert." {
		t.Fatalf("prompt = %q", threads.turns[1].Prompt)
	}

	threads.set(handoffcontinue.ErrThreadBusy, false)
	response = h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"again"}`, bearer)
	if response.Code != http.StatusConflict || decode(t, response)["code"] != "thread_busy" {
		t.Fatalf("busy: status = %d, body = %s", response.Code, response.Body.String())
	}

	threads.set(workspacehandoff.ErrNotFound, false)
	if response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"again"}`, bearer); response.Code != http.StatusUnauthorized {
		t.Fatalf("gone: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestARotationKeepsTheThreadTarget(t *testing.T) {
	h, _ := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	mintThreadTrigger(t, h, versionID, thread, map[string]any{"grant": map[string]any{"continue_step_limit": true}})

	response := h.do(t, http.MethodPost, triggerRoute(versionID), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("rotate: status = %d, body = %s", response.Code, response.Body.String())
	}
	view := decode(t, response)
	grant, _ := view["grant"].(map[string]any)
	if view["target_kind"] != "workspace_thread" || view["conversation_uuid"] != thread ||
		view["prompt"] != "Triage the alert." || grant["continue_step_limit"] != true {
		t.Fatalf("a bodyless rotation lost the thread target: %v", view)
	}

	// Naming the version's own kind clears the thread target.
	response = h.do(t, http.MethodPost, triggerRoute(versionID), `{"target_kind":"pipeline"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("to pipeline: status = %d, body = %s", response.Code, response.Body.String())
	}
	if view := decode(t, response); view["target_kind"] != "pipeline" || view["conversation_uuid"] != nil {
		t.Fatalf("view = %v", view)
	}
}

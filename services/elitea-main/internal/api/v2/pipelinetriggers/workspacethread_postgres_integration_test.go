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
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// fakeThreadRuns is the handed-off thread seam.
type fakeThreadRuns struct {
	mu        sync.Mutex
	handedOff bool
	// thread is what HandedOffThread answers.
	thread    handoffcontinue.ThreadTarget
	active    bool
	activeIDs []string
	err       error
	turns     []handoffcontinue.ThreadTurn
}

func (f *fakeThreadRuns) HandedOffThread(context.Context, int64, string) (handoffcontinue.ThreadTarget, bool, error) {
	return f.thread, f.handedOff, nil
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
	installApprovalTables(t, h.pool)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "* * * * *", "active": true, "input": "ignored", "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.", "on_approval_required": "skip",
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
	// The run's approval snapshot goes with the start, for the admission
	// to write behind the CLOUD_IDLE fence: the schedule's own action.
	if run := turn.Run; run == nil || run.ScheduleID == 0 || run.TriggerID != 0 || run.Policy == nil ||
		run.Policy.OnApprovalRequired != "skip" || run.Policy.OwnerUserID != ownerUserID ||
		run.Policy.VersionID != versionID {
		t.Fatalf("run = %+v", turn.Run)
	}
	var policies int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_runtime.unattended_run_policies`).Scan(&policies); err != nil || policies != 0 {
		t.Fatalf("the handler wrote a policy row itself: %d, %v", policies, err)
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
	// The busy skip uses up the occurrence: it stamps last_run (the one
	// exception to 0133's no-stamp rule), and keeps the last turn's id.
	if view["last_result"] != "skipped_thread_busy" || view["last_run"] == firstRun ||
		view["last_execution_id"] != "thread-exec-1" {
		t.Fatalf("busy view = %v", view)
	}
	busyAudits := func() int {
		n := 0
		for _, event := range h.recorder.all() {
			if strings.HasPrefix(event.Action, "pipeline schedule skipped_thread_busy") {
				n++
			}
		}
		return n
	}
	busyAt := view["last_result_at"]
	// A second tick in the same cron slot: not due, so no second result and
	// no second audit row, although the thread is still busy.
	result, err = h.handler.RunDueSchedules(context.Background(), now.Add(4*time.Minute+30*time.Second))
	if err != nil || result.Considered != 0 {
		t.Fatalf("same-slot tick = %+v, %v", result, err)
	}
	if view := h.readSchedule(t, versionID); view["last_result_at"] != busyAt || busyAudits() != 1 {
		t.Fatalf("same slot: view = %v, busy audits = %d", view, busyAudits())
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
		turn.ActorUserID != ownerUserID || turn.Grant == nil || !turn.Grant.GoalCheck ||
		turn.Run == nil || turn.Run.TriggerID == 0 || turn.Run.ScheduleID != 0 {
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

// seedTriggerQuestions writes n questions of the thread that the trigger
// started (the `trigger_id` meta its admission stamps), created now.
func seedTriggerQuestions(t *testing.T, h *harness, thread string, triggerID int64, n int) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group (uuid, conversation_id, author_participant_id, is_streaming, meta, created_at)
SELECT gen_random_uuid(), conversation.id, mapping.participant_id, FALSE,
       jsonb_build_object('trigger_id', $2::bigint), now() AT TIME ZONE 'UTC'
  FROM %[1]s.chat_conversations AS conversation
  JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
 CROSS JOIN generate_series(1, $3)
 WHERE conversation.uuid = $1::uuid`, homeSchema), thread, triggerID, n); err != nil {
		t.Fatalf("seed trigger questions: %v", err)
	}
}

func triggerIDOf(t *testing.T, h *harness, versionID int64) int64 {
	t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT id FROM %s.pipeline_triggers WHERE version_id = $1`, homeSchema), versionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A thread trigger has the agent trigger's run limit: 30 turns in 10
// minutes, and 4 answers streaming, with the same 429 and Retry-After.
func TestAThreadTriggerHasTheAgentRunLimit(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	tokenID, secret := mintThreadTrigger(t, h, versionID, thread, nil)
	bearer := map[string]string{"Authorization": "Bearer " + secret}
	triggerID := triggerIDOf(t, h, versionID)

	// 29 started turns in the window: the 30th call is admitted.
	seedTriggerQuestions(t, h, thread, triggerID, 29)
	// Another trigger's turns do not count.
	seedTriggerQuestions(t, h, thread, triggerID+1000, 10)
	if response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", bearer); response.Code != http.StatusAccepted {
		t.Fatalf("under the limit: status = %d, body = %s", response.Code, response.Body.String())
	}
	seedTriggerQuestions(t, h, thread, triggerID, 1)
	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", bearer)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("over the window: status = %d, body = %s", response.Code, response.Body.String())
	}
	if threads.count() != 1 {
		t.Fatalf("turns = %d, want 1", threads.count())
	}
	// Old turns leave the window.
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE %s.chat_message_group SET created_at = created_at - interval '11 minutes'`, homeSchema)); err != nil {
		t.Fatal(err)
	}
	if response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", bearer); response.Code != http.StatusAccepted {
		t.Fatalf("after the window: status = %d, body = %s", response.Code, response.Body.String())
	}
	// Four answers that stream: refused.
	seedStreamingResponse(t, h.pool, homeSchema, thread)
	seedStreamingResponse(t, h.pool, homeSchema, thread)
	seedStreamingResponse(t, h.pool, homeSchema, thread)
	seedStreamingResponse(t, h.pool, homeSchema, thread)
	if response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", bearer); response.Code != http.StatusTooManyRequests {
		t.Fatalf("in flight: status = %d, body = %s", response.Code, response.Body.String())
	}
}

// Under credential governance a thread target must address an agent at the
// row's version; a model thread is refused at save time.
func TestAGovernedThreadTargetNeedsTheVersionsAgent(t *testing.T) {
	g := newGovernedHarness(t)
	threads := &fakeThreadRuns{handedOff: true}
	g.handler = pipelinetriggers.NewPlatformHandler(
		g.pool, g.start, g.vault,
		multiUserPermissions{users: map[int64]bool{ownerUserID: true, subjectUserID: true}},
		g.recorder, nil, nil, nil,
		pipelinetriggers.WithGovernedRuns(g.mandates.Service()),
		pipelinetriggers.WithThreadRuns(threads),
	)
	g.router = mountRoutes(g.handler)
	h := g.harness
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	body := threadBody(t, map[string]any{
		"cron": "0 7 * * *", "target_kind": "workspace_thread", "conversation_uuid": thread, "prompt": "Go.",
	})
	trigger := threadBody(t, map[string]any{"target_kind": "workspace_thread", "conversation_uuid": thread, "prompt": "Go."})

	for name, participant := range map[string]handoffcontinue.ThreadTarget{
		"model thread":  {Target: handoffcontinue.Target{ConversationUUID: thread, ParticipantID: 3}},
		"other version": {Target: handoffcontinue.Target{ConversationUUID: thread, ParticipantID: 3, Agent: true}, VersionID: versionID + 1},
	} {
		threads.thread = participant
		for route, payload := range map[string]string{scheduleRoute(versionID): body} {
			response := h.do(t, http.MethodPut, route, payload, nil)
			if response.Code != http.StatusUnprocessableEntity || decode(t, response)["code"] != "thread_not_governable" {
				t.Fatalf("%s schedule: status = %d, body = %s", name, response.Code, response.Body.String())
			}
		}
		response := h.do(t, http.MethodPost, triggerRoute(versionID), trigger, nil)
		if response.Code != http.StatusUnprocessableEntity || decode(t, response)["code"] != "thread_not_governable" {
			t.Fatalf("%s trigger: status = %d, body = %s", name, response.Code, response.Body.String())
		}
	}
	if view := h.readSchedule(t, versionID); view["configured"] != false {
		t.Fatalf("a refused save stored a schedule: %v", view)
	}

	threads.thread = handoffcontinue.ThreadTarget{
		Target: handoffcontinue.Target{ConversationUUID: thread, ParticipantID: 3, Agent: true}, VersionID: versionID,
	}
	if response := h.do(t, http.MethodPut, scheduleRoute(versionID), body, nil); response.Code != http.StatusOK {
		t.Fatalf("the version's agent: status = %d, body = %s", response.Code, response.Body.String())
	}
}

// removeFromThread drops the user's participant mapping from the thread.
func removeFromThread(t *testing.T, h *harness, thread string, user int64) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(`
DELETE FROM %[1]s.chat_participant_mapping AS mapping
 USING %[1]s.chat_conversations AS conversation, %[1]s.chat_participants AS participant
 WHERE mapping.conversation_id = conversation.id AND participant.id = mapping.participant_id
   AND conversation.uuid = $1::uuid AND participant.entity_name = 'user'
   AND participant.entity_meta ->> 'id' = $2::text`, homeSchema), thread, fmt.Sprint(user)); err != nil {
		t.Fatalf("remove from thread: %v", err)
	}
}

// The right to write to the thread is checked again at each occurrence: an
// author removed from the thread stops the schedule (skipped_unauthorized)
// and the webhook (the refusal for a missing permission).
func TestAThreadOccurrenceChecksTheWriterAgain(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	if response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "* * * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.",
	}), nil); response.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body = %s", response.Code, response.Body.String())
	}
	tokenID, secret := mintThreadTrigger(t, h, versionID, thread, nil)
	removeFromThread(t, h, thread, ownerUserID)

	result, err := h.handler.RunDueSchedules(context.Background(), time.Now().UTC().Truncate(time.Minute).Add(5*time.Second))
	if err != nil || result.Skipped != 1 || threads.count() != 0 {
		t.Fatalf("tick = %+v, %v, turns = %d", result, err, threads.count())
	}
	if view := h.readSchedule(t, versionID); view["last_result"] != "skipped_unauthorized" ||
		!strings.Contains(fmt.Sprint(view["last_result_detail"]), "participant") {
		t.Fatalf("view = %v", view)
	}
	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), "", map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusUnauthorized || threads.count() != 0 {
		t.Fatalf("webhook: status = %d, body = %s", response.Code, response.Body.String())
	}

	// A conversation that is gone keeps the existing missing result.
	if _, err := h.pool.Exec(context.Background(), fmt.Sprintf(
		`DELETE FROM %s.chat_conversations WHERE uuid = $1::uuid`, homeSchema), thread); err != nil {
		t.Fatal(err)
	}
	if _, err := h.handler.RunDueSchedules(context.Background(), time.Now().UTC().Truncate(time.Minute).Add(65*time.Second)); err != nil {
		t.Fatal(err)
	}
	if view := h.readSchedule(t, versionID); view["last_result"] != "skipped_missing_version" {
		t.Fatalf("gone: view = %v", view)
	}
}

// A thread trigger with a provider preset takes the agent trigger's default
// event list.
func TestAThreadTriggerTakesTheAgentDefaultEvents(t *testing.T) {
	h, _ := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	response := h.do(t, http.MethodPost, triggerRoute(versionID), threadBody(t, map[string]any{
		"type": "github", "target_kind": "workspace_thread", "conversation_uuid": thread, "prompt": "Review.",
	}), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint: status = %d, body = %s", response.Code, response.Body.String())
	}
	events, _ := json.Marshal(decode(t, response)["events"])
	if string(events) != `["push","pull_request"]` {
		t.Fatalf("events = %s", events)
	}
}

// A save that keeps the target and leaves the schedule paused does not
// check the thread again: a schedule whose thread was erased can be paused.
// Setting it active checks the thread.
func TestAPauseDoesNotCheckTheThreadAgain(t *testing.T) {
	h, threads := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, h, ownerUserID)
	if response := h.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "0 7 * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Run the nightly checks.",
	}), nil); response.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body = %s", response.Code, response.Body.String())
	}
	threads.mu.Lock()
	threads.handedOff = false // the thread was erased
	threads.mu.Unlock()

	response := h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 8 * * *","active":false}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("pause: status = %d, body = %s", response.Code, response.Body.String())
	}
	if view := decode(t, response); view["active"] != false || view["target_kind"] != "workspace_thread" ||
		view["conversation_uuid"] != thread || view["cron"] != "0 8 * * *" {
		t.Fatalf("view = %v", view)
	}
	response = h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 8 * * *","active":true}`, nil)
	if response.Code != http.StatusUnprocessableEntity || decode(t, response)["code"] != "not_handed_off" {
		t.Fatalf("resume: status = %d, body = %s", response.Code, response.Body.String())
	}
}

// A target_kind that the row cannot hold is refused: `agent` on a
// schedule, and on a trigger the kind that is not the version's.
func TestATargetKindMustFitTheRow(t *testing.T) {
	h, _ := newThreadHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Pipeline", ownerUserID)
	response := h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 7 * * *","target_kind":"agent"}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("agent schedule: status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := h.do(t, http.MethodPut, scheduleRoute(versionID), `{"cron":"0 7 * * *","target_kind":"pipeline"}`, nil); response.Code != http.StatusOK {
		t.Fatalf("pipeline schedule: status = %d, body = %s", response.Code, response.Body.String())
	}
	response = h.do(t, http.MethodPost, triggerRoute(versionID), `{"target_kind":"agent"}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("agent trigger on a pipeline: status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := h.do(t, http.MethodPost, triggerRoute(versionID), `{"target_kind":"pipeline"}`, nil); response.Code != http.StatusOK {
		t.Fatalf("pipeline trigger: status = %d, body = %s", response.Code, response.Body.String())
	}
}

// newGovernedThreadHarness is the governed harness with the thread seam and
// a thread whose next turn addresses an agent at the version, under an
// attached autonomous mandate.
func newGovernedThreadHarness(t *testing.T) (*governedHarness, *fakeThreadRuns, int64) {
	t.Helper()
	g := newGovernedHarness(t)
	threads := &fakeThreadRuns{handedOff: true}
	g.handler = pipelinetriggers.NewPlatformHandler(
		g.pool, g.start, g.vault,
		multiUserPermissions{users: map[int64]bool{ownerUserID: true, subjectUserID: true}},
		g.recorder, nil, nil, nil,
		pipelinetriggers.WithGovernedRuns(g.mandates.Service()),
		pipelinetriggers.WithThreadRuns(threads),
	)
	g.router = mountRoutes(g.handler)
	versionID := seedPipeline(t, g.pool, homeSchema, "Thread anchor", ownerUserID)
	thread := seedThread(t, g.harness, ownerUserID)
	threads.thread = handoffcontinue.ThreadTarget{
		Target: handoffcontinue.Target{ConversationUUID: thread, ParticipantID: 3, Agent: true}, VersionID: versionID,
	}
	if response := g.do(t, http.MethodPut, scheduleRoute(versionID), threadBody(t, map[string]any{
		"cron": "* * * * *", "active": true, "target_kind": "workspace_thread",
		"conversation_uuid": thread, "prompt": "Go.",
	}), nil); response.Code != http.StatusOK {
		t.Fatalf("save: status = %d, body = %s", response.Code, response.Body.String())
	}
	g.mandates.PutBinding("binding-1", 1, versionID)
	g.mandates.PutMandate("auto", 1, mandate.KindAutonomous, "binding-1", 0)
	g.attachSchedule(t, versionID, "auto")
	return g, threads, versionID
}

// The start's ErrThreadVersion skip under a mandate is a mandate refusal:
// the security audit records MANDATE_SCOPE_MISMATCH, as the fire-time
// check's refusals do.
func TestAGovernedThreadVersionSkipIsAudited(t *testing.T) {
	g, threads, versionID := newGovernedThreadHarness(t)
	threads.set(handoffcontinue.ErrThreadVersion, false)
	result, err := g.handler.RunDueSchedules(context.Background(), governedNow())
	if err != nil || result.Skipped != 1 {
		t.Fatalf("tick = %+v, %v", result, err)
	}
	if view := g.readSchedule(t, versionID); view["last_result"] != "skipped_mandate_scope" {
		t.Fatalf("view = %v", view)
	}
	refusals := g.mandates.Refusals()
	if len(refusals) != 1 || refusals[0].Code != runmandate.CodeMandateScopeMismatch ||
		refusals[0].MandateID != "auto" || refusals[0].Target.Origin != runmandate.OriginPipelineSchedule {
		t.Fatalf("refusals = %+v", refusals)
	}
}

// A run identity that is not a user (an autonomous mandate on a row with no
// author) is refused before the start, so no turn runs with the previous
// turn's policy row.
func TestAThreadOccurrenceWithNoIdentityIsRefused(t *testing.T) {
	g, threads, versionID := newGovernedThreadHarness(t)
	if _, err := g.pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE %s.pipeline_schedules SET author_id = 0 WHERE version_id = $1`, homeSchema), versionID); err != nil {
		t.Fatal(err)
	}
	result, err := g.handler.RunDueSchedules(context.Background(), governedNow())
	if err != nil || result.Skipped != 1 || threads.count() != 0 {
		t.Fatalf("tick = %+v, %v, turns = %d", result, err, threads.count())
	}
	// Refused as a run identity, before the thread's writer check.
	if view := g.readSchedule(t, versionID); view["last_result"] != "skipped_unauthorized" ||
		strings.Contains(fmt.Sprint(view["last_result_detail"]), "participant") {
		t.Fatalf("view = %v", view)
	}
}

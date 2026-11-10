package repos

// The heartbeat of proactive agents against a real PostgreSQL (ADR-0050
// decision 2, issue 1618): each triage outcome, the daily limits, quiet
// hours, the fallback with no mandate, the kill switch, maintenance and the
// governance rule. The real heartbeat.Service drives the real store; only
// the triage model, the run start and the kill switch are doubles.
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/heartbeat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const heartbeatTestExecution = "0123456789abcdef0123456789abcdef"

type heartbeatTestCompleter struct {
	mu       sync.Mutex
	answer   string
	requests []runbrief.CompletionRequest
}

func (c *heartbeatTestCompleter) Complete(_ context.Context, request runbrief.CompletionRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	return c.answer, nil
}

type heartbeatTestStarter struct {
	mu        sync.Mutex
	requests  []agentapp.CurrentApplicationStartRequest
	authority []securitycontext.RunAuthority
}

func (s *heartbeatTestStarter) StartCurrentApplication(
	ctx context.Context, request agentapp.CurrentApplicationStartRequest,
) (agentapp.CurrentApplicationStartOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	authority, _ := securitycontext.RunAuthorityFrom(ctx)
	s.authority = append(s.authority, authority)
	return agentapp.CurrentApplicationStartOutcome{ExecutionID: heartbeatTestExecution, Created: true}, nil
}

type heartbeatTestGate struct{ err error }

func (g heartbeatTestGate) Check(context.Context, safety.Subject) error { return g.err }

type heartbeatHarness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *AgentHeartbeats
	completer *heartbeatTestCompleter
	starter   *heartbeatTestStarter
	mandates  *runmandatetest.Store
	gate      heartbeatTestGate
	now       time.Time
}

// newHeartbeatHarness publishes agent 31 (release of version 42) with the
// proactivity block, turns its heartbeat on for owner 11 and makes project
// 1 the owner's personal project unless personal is false.
func newHeartbeatHarness(t *testing.T, block map[string]any, personal bool) *heartbeatHarness {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	meta, err := json.Marshal(map[string]any{"proactivity": block})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.application_versions SET meta = $1::jsonb WHERE id = 41`, string(meta)); err != nil {
		t.Fatal(err)
	}
	releases, _ := newReleaseTestStores(t, pool)
	releaseTestPublish(t, pool, releases, 41, 42, "v1")
	// The heartbeat reads the goals of the RELEASED version.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.application_versions
SET soul = '{"proactive_goals":["Answer every open question of the team"]}'::jsonb WHERE id = 42`); err != nil {
		t.Fatal(err)
	}
	name := "team"
	if personal {
		name = "project_user_11"
	}
	if _, err := pool.Exec(t.Context(), `
SELECT setval(pg_get_serial_sequence('p_1.chat_conversations', 'id'), 100);
SELECT setval(pg_get_serial_sequence('p_1.chat_participants', 'id'), 100);
ALTER TABLE centry.project ADD COLUMN IF NOT EXISTS name text;
UPDATE centry.project SET name = '`+name+`' WHERE id = 1;
INSERT INTO p_1.agent_heartbeat_settings (application_id, enabled, owner_user_id, next_wake_at)
VALUES (31, TRUE, 11, clock_timestamp() - interval '1 minute');`); err != nil {
		t.Fatal(err)
	}
	// A new message of the team in the agent's conversation.
	if _, err := pool.Exec(t.Context(), `
WITH message AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
    VALUES (gen_random_uuid(), 20, 1, '{}'::jsonb, FALSE, now() AT TIME ZONE 'UTC' - interval '5 minutes')
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, id, now() AT TIME ZONE 'UTC' FROM message
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content) SELECT id, 'Can somebody check the failed nightly build?' FROM item`); err != nil {
		t.Fatal(err)
	}
	return &heartbeatHarness{
		t: t, pool: pool, store: NewAgentHeartbeats(pool),
		completer: &heartbeatTestCompleter{answer: `{"decision":"no_op","reason":"Nothing needs me."}`},
		starter:   &heartbeatTestStarter{}, mandates: runmandatetest.New(), now: time.Now().UTC(),
	}
}

func (h *heartbeatHarness) scan() heartbeat.PassResult {
	h.t.Helper()
	service, err := heartbeat.New(heartbeat.Config{
		Store: h.store, Start: h.starter, Mandates: h.mandates.Service(), Gate: h.gate, Completer: h.completer,
		Now: func() time.Time { return h.now }, Unit: func() float64 { return 0 },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := service.Scan(h.t.Context())
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	return result
}

func (h *heartbeatHarness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.t.Context(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *heartbeatHarness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.t.Context(), sql, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

type heartbeatRow struct {
	outcome, reasonCode, reason string
	executionID, conversation   *string
	pausedUntil                 *time.Time
}

func (h *heartbeatHarness) rows() []heartbeatRow {
	h.t.Helper()
	result, err := h.pool.Query(h.t.Context(), `
SELECT outcome, reason_code, reason, execution_id, conversation_uuid::text, paused_until
FROM elitea_runtime.agent_heartbeats WHERE project_id = 1 AND application_id = 31 ORDER BY id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer result.Close()
	var rows []heartbeatRow
	for result.Next() {
		var row heartbeatRow
		if err := result.Scan(&row.outcome, &row.reasonCode, &row.reason, &row.executionID, &row.conversation,
			&row.pausedUntil); err != nil {
			h.t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func (h *heartbeatHarness) onlyRow() heartbeatRow {
	h.t.Helper()
	rows := h.rows()
	if len(rows) != 1 {
		h.t.Fatalf("want one heartbeat row, got %+v", rows)
	}
	return rows[0]
}

func (h *heartbeatHarness) settings() HeartbeatSettings {
	h.t.Helper()
	settings, err := h.store.GetHeartbeatSettings(h.t.Context(), 1, 31)
	if err != nil {
		h.t.Fatal(err)
	}
	return settings
}

// homeMessages answers the texts of the heartbeat messages in the home
// thread, and the notifications of owner 11.
func (h *heartbeatHarness) homeMessages() ([]string, int) {
	h.t.Helper()
	rows, err := h.pool.Query(h.t.Context(), `
SELECT text.content
FROM p_1.chat_message_group AS message
JOIN p_1.chat_conversations AS conversation ON conversation.id = message.conversation_id
JOIN p_1.chat_message_items AS item ON item.message_group_id = message.id
JOIN p_1.chat_messages_text AS text ON text.id = item.id
WHERE conversation.source = 'agent_heartbeat' AND message.cause = 'heartbeat'
ORDER BY message.id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			h.t.Fatal(err)
		}
		texts = append(texts, text)
	}
	return texts, h.count(`SELECT count(*) FROM centry.notifications WHERE user_id = 11 AND event_type = 'agent_heartbeat'`)
}

func (h *heartbeatHarness) attachMandate() {
	h.mandates.PutBinding("binding-1", 1, 42)
	h.exec(`UPDATE p_1.agent_heartbeat_settings SET mandate_id = '7d1f6a52-0c55-4d5e-9a3e-6f0b1c2d3e4f' WHERE application_id = 31`)
	h.mandates.PutMandate("7d1f6a52-0c55-4d5e-9a3e-6f0b1c2d3e4f", 1, mandate.KindDelegatedBackground, "binding-1", 11)
}

func TestAgentHeartbeat_NoOpWritesOnlyARow(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "interval": "1h"}, true)
	result := h.scan()
	if result.Due != 1 || result.Woken != 1 {
		t.Fatalf("pass = %+v", result)
	}
	row := h.onlyRow()
	if row.outcome != heartbeat.OutcomeNoOp || row.reason != "Nothing needs me." {
		t.Fatalf("row = %+v", row)
	}
	texts, notifications := h.homeMessages()
	if len(texts) != 0 || notifications != 0 {
		t.Fatalf("a no_op posted %v and %d notifications", texts, notifications)
	}
	// The digest carried the goal and the new message, attributed to the wake.
	request := h.completer.requests[0]
	if !strings.Contains(request.User, "failed nightly build") || !strings.Contains(request.User, "Answer every open question") {
		t.Fatalf("the digest lost its material: %s", request.User)
	}
	if !strings.HasPrefix(request.AttributionID, heartbeat.AttributionPrefix) || request.UserID != 11 {
		t.Fatalf("triage request = %+v", request)
	}
	settings := h.settings()
	if !settings.NextWakeAt.After(h.now.Add(50*time.Minute)) || settings.LastWakeAt == nil || settings.ReleaseID == nil {
		t.Fatalf("the clock did not move: %+v", settings)
	}
	// The cursor moved past the message: the next digest is empty.
	h.exec(`UPDATE p_1.agent_heartbeat_settings SET next_wake_at = clock_timestamp() - interval '1 minute'`)
	h.scan()
	if second := h.completer.requests[1].User; strings.Contains(second, "failed nightly build") {
		t.Fatalf("the second digest read the same message again: %s", second)
	}
}

func TestAgentHeartbeat_NotifyPostsToTheHomeThread(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.completer.answer = `{"decision":"notify","reason":"A build failed.","message":"The nightly build failed. Somebody asked for a check."}`
	h.scan()
	row := h.onlyRow()
	if row.outcome != heartbeat.OutcomeNotify || row.conversation == nil {
		t.Fatalf("row = %+v", row)
	}
	texts, notifications := h.homeMessages()
	if len(texts) != 1 || !strings.Contains(texts[0], "nightly build failed") || notifications != 1 {
		t.Fatalf("home thread %v, notifications %d", texts, notifications)
	}
	settings := h.settings()
	if settings.HomeConversationUUID == nil || *settings.HomeConversationUUID != *row.conversation {
		t.Fatalf("home thread not stored: %+v", settings)
	}
	var meta string
	if err := h.pool.QueryRow(t.Context(), `SELECT meta::text FROM centry.notifications WHERE event_type = 'agent_heartbeat'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, *row.conversation) || !strings.Contains(meta, `"application_id": 31`) || !strings.Contains(meta, `Open the conversation]()`) {
		t.Fatalf("notification meta = %s", meta)
	}
	// A second notify reuses the home thread.
	h.exec(`UPDATE p_1.agent_heartbeat_settings SET next_wake_at = clock_timestamp() - interval '1 minute'`)
	h.scan()
	if n := h.count(`SELECT count(*) FROM p_1.chat_conversations WHERE source = 'agent_heartbeat'`); n != 1 {
		t.Fatalf("home threads = %d", n)
	}
}

func TestAgentHeartbeat_ActStartsAnUnattendedRunUnderTheOwnersMandate(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.attachMandate()
	h.completer.answer = `{"decision":"act","reason":"The build failed.","message":"Read the build log and post the cause."}`
	h.scan()
	row := h.onlyRow()
	if row.outcome != heartbeat.OutcomeAct || row.executionID == nil || *row.executionID != heartbeatTestExecution {
		t.Fatalf("row = %+v", row)
	}
	if len(h.starter.requests) != 1 {
		t.Fatalf("starts = %d", len(h.starter.requests))
	}
	request := h.starter.requests[0]
	if !request.Heartbeat || request.ReleaseID == 0 || request.Unattended == nil || request.ActorUserID != 11 ||
		request.ConversationUUID != *row.conversation || !strings.Contains(request.UserInput, "Read the build log") {
		t.Fatalf("start request = %+v", request)
	}
	if h.starter.authority[0].SubjectUserID != 11 || h.starter.authority[0].MandateID == "" {
		t.Fatalf("authority = %+v", h.starter.authority[0])
	}
	if n := h.count(`
SELECT count(*) FROM elitea_runtime.unattended_run_policies
WHERE project_id = 1 AND conversation_uuid = $1::uuid AND trigger_origin = 'schedule'
  AND on_approval_required = 'notify_and_wait' AND owner_user_id = 11 AND version_id = 42`, *row.conversation); n != 1 {
		t.Fatalf("notify_and_wait policy rows = %d", n)
	}
}

func TestAgentHeartbeat_ActWithoutMandateDegradesToNotify(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.completer.answer = `{"decision":"act","reason":"The build failed.","message":"Read the build log."}`
	h.scan()
	row := h.onlyRow()
	if row.outcome != heartbeat.OutcomeActDegraded || row.reasonCode != heartbeat.ReasonMandateRequired || row.executionID != nil {
		t.Fatalf("row = %+v", row)
	}
	if len(h.starter.requests) != 0 {
		t.Fatal("a run started with no mandate")
	}
	texts, notifications := h.homeMessages()
	if len(texts) != 1 || !strings.Contains(texts[0], "I have no mandate") || notifications != 1 {
		t.Fatalf("home thread %v, notifications %d", texts, notifications)
	}
	if refusals := h.mandates.Refusals(); len(refusals) != 1 || refusals[0].Target.Origin != heartbeat.OriginAgentHeartbeat {
		t.Fatalf("refusals = %+v", refusals)
	}
}

func TestAgentHeartbeat_MandateOfAnotherPersonDegrades(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.mandates.PutBinding("binding-1", 1, 42)
	id := uuid.NewString()
	h.mandates.PutMandate(id, 1, mandate.KindDelegatedBackground, "binding-1", 99)
	h.exec(`UPDATE p_1.agent_heartbeat_settings SET mandate_id = $1::uuid`, id)
	h.completer.answer = `{"decision":"act","reason":"r","message":"m"}`
	h.scan()
	if row := h.onlyRow(); row.outcome != heartbeat.OutcomeActDegraded || row.reasonCode != heartbeat.ReasonMandateNotOwner {
		t.Fatalf("row = %+v", row)
	}
}

func TestAgentHeartbeat_WakeLimitPausesUntilTheNextDay(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "limits": map[string]any{"wakes_per_day": 2}}, true)
	for range 2 {
		h.exec(`INSERT INTO elitea_runtime.agent_heartbeats (wake_id, project_id, application_id, owner_user_id, outcome)
VALUES (gen_random_uuid(), 1, 31, 11, 'no_op')`)
	}
	h.scan()
	if len(h.completer.requests) != 0 {
		t.Fatal("a wake over the limit called the model")
	}
	rows := h.rows()
	last := rows[len(rows)-1]
	if len(rows) != 3 || last.outcome != heartbeat.OutcomePaused || last.reasonCode != heartbeat.ReasonWakeLimit ||
		last.pausedUntil == nil || !last.pausedUntil.After(h.now) {
		t.Fatalf("rows = %+v", rows)
	}
	settings := h.settings()
	if settings.PausedUntil == nil || !settings.NextWakeAt.Equal(*settings.PausedUntil) {
		t.Fatalf("settings = %+v", settings)
	}
	texts, notifications := h.homeMessages()
	if len(texts) != 1 || !strings.Contains(texts[0], "daily wake limit") || notifications != 1 {
		t.Fatalf("home thread %v, notifications %d", texts, notifications)
	}
}

func TestAgentHeartbeat_SpendLimitCountsTheTriageAndTheActRuns(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "limits": map[string]any{"spend_per_day_usd": 1.5}}, true)
	wake := uuid.NewString()
	h.exec(`INSERT INTO elitea_runtime.agent_heartbeats (wake_id, project_id, application_id, owner_user_id, outcome, execution_id)
VALUES ($1::uuid, 1, 31, 11, 'act', $2)`, wake, heartbeatTestExecution)
	for _, spend := range []struct {
		id  string
		usd float64
	}{{heartbeat.AttributionPrefix + wake, 0.5}, {heartbeatTestExecution, 0.75}, {"goal_check:" + heartbeatTestExecution, 0.25}, {"unrelated", 50}} {
		h.exec(`
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms, provider, model, execution_id, operation, cost_usd)
VALUES (now(), 1, 11, '/tools', 'POST', 200, 10, 'firecrawl', 'scrape', $1, 'tool', $2)`, spend.id, spend.usd)
	}
	day, err := h.store.DayEnvelope(t.Context(), heartbeat.Agent{ProjectID: 1, ApplicationID: 31}, h.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if day.Wakes != 1 || day.Acts != 1 || fmt.Sprintf("%.2f", day.SpendUSD) != "1.50" {
		t.Fatalf("day = %+v", day)
	}
	h.scan()
	rows := h.rows()
	if last := rows[len(rows)-1]; last.outcome != heartbeat.OutcomePaused || last.reasonCode != heartbeat.ReasonSpendLimit {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAgentHeartbeat_ActLimitDegradesToNotify(t *testing.T) {
	// A personal project: the platform's personal_acts_per_day (2) is the ceiling.
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "limits": map[string]any{"acts_per_day": 4}}, true)
	h.attachMandate()
	for range 2 {
		h.exec(`INSERT INTO elitea_runtime.agent_heartbeats (wake_id, project_id, application_id, owner_user_id, outcome, execution_id)
VALUES (gen_random_uuid(), 1, 31, 11, 'act', $1)`, heartbeatTestExecution)
	}
	h.completer.answer = `{"decision":"act","reason":"r","message":"Do it."}`
	h.scan()
	rows := h.rows()
	if last := rows[len(rows)-1]; last.outcome != heartbeat.OutcomeActDegraded || last.reasonCode != heartbeat.ReasonActLimit {
		t.Fatalf("rows = %+v", rows)
	}
	if len(h.starter.requests) != 0 {
		t.Fatal("a run started over the act limit")
	}
}

func TestAgentHeartbeat_QuietHoursSkipWithoutARow(t *testing.T) {
	now := time.Now().UTC()
	window := fmt.Sprintf("%s-%s UTC", now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04"))
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "quiet_hours": window}, true)
	h.now = now
	h.scan()
	if len(h.rows()) != 0 || len(h.completer.requests) != 0 {
		t.Fatal("a wake in quiet hours was triaged")
	}
	settings := h.settings()
	if settings.LastSkipReason != heartbeat.ReasonQuietHours || !settings.NextWakeAt.After(now.Add(50*time.Minute)) {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestAgentHeartbeat_KillSwitchSkips(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.gate = heartbeatTestGate{err: &safety.SuspendedError{SuspensionID: 1}}
	h.scan()
	if len(h.rows()) != 0 || len(h.completer.requests) != 0 {
		t.Fatal("a suspended agent was triaged")
	}
	if settings := h.settings(); settings.LastSkipReason != heartbeat.ReasonSuspended {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestAgentHeartbeat_MaintenanceAndProjectSwitchSuppress(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.exec(`
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key));
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('maintenance', 'maintenance_enabled', 'true'::jsonb, now() AT TIME ZONE 'UTC')`)
	if result := h.scan(); !result.Suppressed || result.Due != 0 {
		t.Fatalf("pass in maintenance = %+v", result)
	}
	h.exec(`DELETE FROM centry.platform_config`)
	if err := h.store.SetProjectEnabled(t.Context(), 1, 11, false); err != nil {
		t.Fatal(err)
	}
	h.scan()
	if len(h.completer.requests) != 0 || h.settings().LastSkipReason != heartbeat.ReasonProjectOff {
		t.Fatalf("project switch off: settings %+v", h.settings())
	}
}

func TestAgentHeartbeat_TeamProjectNeedsAReviewedMediumTier(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, false)
	h.scan()
	if len(h.completer.requests) != 0 || h.settings().LastSkipReason != heartbeat.ReasonReviewRequired {
		t.Fatalf("low tier woke: %+v", h.settings())
	}
	var releaseID int64
	if err := h.pool.QueryRow(t.Context(), `
SELECT release_id FROM elitea_lifecycle.application_release_heads WHERE tenant_project_id = 1 AND application_id = 31`).Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	h.exec(`
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, risk_tier)
VALUES (1, 31, 'medium')
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET risk_tier = 'medium'`)
	h.exec(`
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, candidate_version_id, version_name,
     summary, proposed_by, required_approvals, release_id)
VALUES (1, 31, 'release', 'published', 'medium', 41, 'v1', 'Reviewed.', 7, 1, $1)`, releaseID)
	h.exec(`UPDATE p_1.agent_heartbeat_settings SET next_wake_at = clock_timestamp() - interval '1 minute'`)
	h.scan()
	if len(h.completer.requests) != 1 {
		t.Fatalf("a reviewed medium-tier agent did not wake: %+v", h.settings())
	}
	governance, err := h.store.HeartbeatGovernanceOf(t.Context(), 1, 31, 11)
	if err != nil || governance.Tier != "medium" || governance.Personal {
		t.Fatalf("governance = %+v, %v", governance, err)
	}
}

func TestAgentHeartbeat_HistoryAndSettingsRoundTrip(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat"}, true)
	h.scan()
	entries, total, err := h.store.ListHeartbeats(t.Context(), 1, 31, 10, 0)
	if err != nil || total != 1 || len(entries) != 1 || entries[0].Outcome != heartbeat.OutcomeNoOp {
		t.Fatalf("history = %+v %d %v", entries, total, err)
	}
	saved, err := h.store.SaveHeartbeatSettings(t.Context(), 1, 31, HeartbeatSettingsUpdate{Enabled: false, OwnerUserID: 11, ActorID: 11})
	if err != nil || saved.Enabled {
		t.Fatalf("disable = %+v %v", saved, err)
	}
	mandateID := uuid.NewString()
	saved, err = h.store.SaveHeartbeatSettings(t.Context(), 1, 31, HeartbeatSettingsUpdate{
		Enabled: true, OwnerUserID: 11, MandateID: &mandateID, ActorID: 11,
	})
	if err != nil || !saved.Enabled || saved.MandateID == nil || *saved.MandateID != mandateID || saved.NextWakeAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("enable = %+v %v", saved, err)
	}
	saved, err = h.store.SaveHeartbeatSettings(t.Context(), 1, 31, HeartbeatSettingsUpdate{Enabled: true, OwnerUserID: 11, ClearMandate: true})
	if err != nil || saved.MandateID != nil {
		t.Fatalf("clear mandate = %+v %v", saved, err)
	}
	proactive, err := h.store.ProactiveApplicationIDs(t.Context(), 1, []int64{31, 32})
	if err != nil || !proactive[31] || proactive[32] {
		t.Fatalf("proactive = %v %v", proactive, err)
	}
}

// Two passes (two replicas) see the same due agent: only the first claim
// takes it, so the agent is triaged once.
func TestAgentHeartbeat_ClaimLetsOnePassWake(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "interval": "1h"}, true)
	agents, err := h.store.DueAgents(t.Context(), 1, h.now, 10)
	if err != nil || len(agents) != 1 {
		t.Fatalf("due = %+v, %v", agents, err)
	}
	first, err := h.store.Claim(t.Context(), agents[0], h.now, h.now.Add(5*time.Minute))
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v", first, err)
	}
	second, err := h.store.Claim(t.Context(), agents[0], h.now, h.now.Add(5*time.Minute))
	if err != nil || second {
		t.Fatalf("second claim = %v, %v", second, err)
	}
	// A pass now finds nothing due: the claim moved the wake.
	if result := h.scan(); result.Due != 0 || len(h.completer.requests) != 0 {
		t.Fatalf("pass = %+v, triage calls %d", result, len(h.completer.requests))
	}
}

// An act parked on an approval is still running: the next wake skips
// instead of piling another act onto the home thread.
func TestAgentHeartbeat_AParkedActSkipsTheWake(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "interval": "1h"}, true)
	h.exec(`
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, meta, created_at)
VALUES (gen_random_uuid(), 'home', 11, 'agent_heartbeat', '{"heartbeat_application_id":"31"}'::jsonb, now() AT TIME ZONE 'UTC');
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
SELECT gen_random_uuid(), 21, id, '{"hitl_interrupt":{"id":"x"}}'::jsonb, FALSE, now() AT TIME ZONE 'UTC'
FROM p_1.chat_conversations WHERE name = 'home'`)
	active, err := h.store.RunActive(t.Context(), heartbeat.Agent{ProjectID: 1, ApplicationID: 31})
	if err != nil || !active {
		t.Fatalf("a parked act = %v, %v", active, err)
	}
	h.scan()
	if len(h.completer.requests) != 0 || len(h.rows()) != 0 {
		t.Fatalf("the wake ran on top of a parked act: %d triage calls", len(h.completer.requests))
	}
}

// Another person's private conversation with the agent never reaches the
// owner's digest; a shared one does.
func TestAgentHeartbeat_TheDigestSkipsOthersPrivateChats(t *testing.T) {
	h := newHeartbeatHarness(t, map[string]any{"mode": "heartbeat", "interval": "1h"}, true)
	h.exec(`
INSERT INTO p_1.chat_conversations (id, uuid, name, author_id, is_private, source, meta, created_at) VALUES
    (90, gen_random_uuid(), 'private of 12', 12, TRUE, 'elitea', '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (91, gen_random_uuid(), 'shared by 12', 12, FALSE, 'elitea', '{}'::jsonb, now() AT TIME ZONE 'UTC');
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta) VALUES
    (95, gen_random_uuid(), 'user', '{"id":12}'::jsonb);
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES
    (90, 95, '{}'::jsonb, now() AT TIME ZONE 'UTC'), (90, 21, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (91, 95, '{}'::jsonb, now() AT TIME ZONE 'UTC'), (91, 21, '{}'::jsonb, now() AT TIME ZONE 'UTC');`)
	for conversation, text := range map[int]string{90: "my salary is secret", 91: "the release is blocked"} {
		h.exec(`
WITH message AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
    VALUES (gen_random_uuid(), 95, $1, '{}'::jsonb, FALSE, now() AT TIME ZONE 'UTC' - interval '4 minutes')
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, id, now() AT TIME ZONE 'UTC' FROM message
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content) SELECT id, $2 FROM item`, conversation, text)
	}
	h.scan()
	digest := h.completer.requests[0].User
	if strings.Contains(digest, "salary") {
		t.Fatalf("another person's private chat reached the digest: %s", digest)
	}
	if !strings.Contains(digest, "release is blocked") || !strings.Contains(digest, "nightly build") {
		t.Fatalf("the digest lost a visible conversation: %s", digest)
	}
}

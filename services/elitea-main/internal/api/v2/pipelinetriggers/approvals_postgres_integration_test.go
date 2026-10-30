package pipelinetriggers_test

// on_approval_required on schedules and triggers, and the run snapshot
// (ADR-0041 decision 5, #1491). Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// installApprovalTables applies the two 0211 tables this package writes,
// taken from the shipped migration (the package's database has the tenant
// corpus only).
func installApprovalTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	migration, err := platformmigrations.Files.ReadFile("shared/0211_approval_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(migration)
	statements := []string{"CREATE SCHEMA IF NOT EXISTS elitea_runtime"}
	for _, table := range []string{"unattended_run_policies", "unattended_approval_settings"} {
		start := strings.Index(text, "CREATE TABLE IF NOT EXISTS elitea_runtime."+table+" (")
		if start < 0 {
			t.Fatalf("0211 has no %s", table)
		}
		end := strings.Index(text[start:], "\n);")
		statements = append(statements, text[start:start+end+3])
	}
	for _, statement := range statements {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("install %q: %v", statement[:40], err)
		}
	}
}

func TestScheduleOnApprovalRequiredIsSavedValidatedAndDefaulted(t *testing.T) {
	h := newHarness(t)
	installApprovalTables(t, h.pool)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	target := fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID)

	h.saveSchedule(t, versionID, "0 3 * * *", true)
	if got := h.readSchedule(t, versionID)["on_approval_required"]; got != "fail" {
		t.Fatalf("default on_approval_required = %v, want fail", got)
	}
	for _, refused := range []string{`"auto_approve"`, `"approve"`, `7`} {
		response := h.do(t, http.MethodPut, target,
			`{"cron":"0 3 * * *","active":true,"on_approval_required":`+refused+`}`, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("on_approval_required %s: status = %d, want 400", refused, response.Code)
		}
	}
	response := h.do(t, http.MethodPut, target, `{"cron":"0 3 * * *","active":true,"on_approval_required":"skip"}`, nil)
	if response.Code != http.StatusOK || decode(t, response)["on_approval_required"] != "skip" {
		t.Fatalf("save skip: status = %d, body = %s", response.Code, response.Body.String())
	}
	// A save that does not name it keeps it.
	h.saveSchedule(t, versionID, "0 4 * * *", true)
	if got := h.readSchedule(t, versionID)["on_approval_required"]; got != "skip" {
		t.Fatalf("after a save without the field = %v, want skip", got)
	}
	// The platform default applies when the schedule has no value.
	if _, err := h.pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('approvals', 'unattended_default', '"notify_and_wait"', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	if response := h.do(t, http.MethodDelete, target, "", nil); response.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", response.Code, response.Body.String())
	}
	h.saveSchedule(t, versionID, "0 3 * * *", true)
	if got := h.readSchedule(t, versionID)["on_approval_required"]; got != "notify_and_wait" {
		t.Fatalf("platform default = %v, want notify_and_wait", got)
	}
}

func TestTriggerRunSnapshotsItsOnApprovalRequired(t *testing.T) {
	h := newHarness(t)
	installApprovalTables(t, h.pool)
	versionID := seedPipeline(t, h.pool, homeSchema, "Inbound", ownerUserID)
	response := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID),
		`{"on_approval_required":"notify_and_wait"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	if body["on_approval_required"] != "notify_and_wait" {
		t.Fatalf("trigger view = %v", body)
	}
	tokenID, _ := body["token_id"].(string)
	secret, _ := body["secret"].(string)
	fired := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"go"}`,
		map[string]string{"Authorization": "Bearer " + secret})
	if fired.Code != http.StatusAccepted {
		t.Fatalf("fire: %d %s", fired.Code, fired.Body.String())
	}
	request, ok := h.start.last()
	if !ok {
		t.Fatal("nothing was dispatched")
	}
	var origin, action string
	var version, owner int64
	if err := h.pool.QueryRow(context.Background(), `
SELECT trigger_origin, version_id, owner_user_id, on_approval_required
FROM elitea_runtime.unattended_run_policies WHERE project_id = 1 AND conversation_uuid = $1::uuid`,
		request.ConversationUUID).Scan(&origin, &version, &owner, &action); err != nil {
		t.Fatalf("run snapshot: %v", err)
	}
	if origin != "webhook" || version != versionID || owner != ownerUserID || action != "notify_and_wait" {
		t.Fatalf("snapshot = %s %d %d %s", origin, version, owner, action)
	}
	// A later change does not change the snapshot of a run in flight.
	rotated := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID),
		`{"on_approval_required":"skip"}`, nil)
	if rotated.Code != http.StatusOK || decode(t, rotated)["on_approval_required"] != "skip" {
		t.Fatalf("rotate: %d %s", rotated.Code, rotated.Body.String())
	}
	if err := h.pool.QueryRow(context.Background(), `
SELECT on_approval_required FROM elitea_runtime.unattended_run_policies
WHERE project_id = 1 AND conversation_uuid = $1::uuid`, request.ConversationUUID).Scan(&action); err != nil ||
		action != "notify_and_wait" {
		t.Fatalf("snapshot after change = %s (%v)", action, err)
	}
}

package pipelinetriggers_test

// A schedule or trigger save and its on_approval_required action are one
// transaction (ADR-0041 decision 5, #1491). Requires
// ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// breakApprovalSettingsWrites makes every write of an approval action fail,
// after the schedule or trigger row of the same request is written.
func breakApprovalSettingsWrites(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, statement := range []string{`
CREATE OR REPLACE FUNCTION elitea_runtime.refuse_approval_setting() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'approval settings are read-only in this test'; END $$`, `
CREATE TRIGGER refuse_approval_setting BEFORE INSERT OR UPDATE ON elitea_runtime.unattended_approval_settings
FOR EACH ROW EXECUTE FUNCTION elitea_runtime.refuse_approval_setting()`} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTriggerRotationWithAFailedApprovalSaveChangesNothing(t *testing.T) {
	h := newHarness(t)
	installApprovalTables(t, h.pool)
	versionID := seedPipeline(t, h.pool, homeSchema, "Inbound", ownerUserID)
	target := fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID)
	minted := h.do(t, http.MethodPost, target, `{"on_approval_required":"notify_and_wait"}`, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", minted.Code, minted.Body.String())
	}
	body := decode(t, minted)
	tokenID, _ := body["token_id"].(string)
	secret, _ := body["secret"].(string)
	vaultEntries := h.vault.size()

	breakApprovalSettingsWrites(t, h.pool)
	rotated := h.do(t, http.MethodPost, target, `{"on_approval_required":"skip"}`, nil)
	if rotated.Code != http.StatusInternalServerError {
		t.Fatalf("rotate with a failing approval save: %d %s, want 500", rotated.Code, rotated.Body.String())
	}
	// Nothing changed: the trigger keeps its credential and its action, and
	// the secret the caller holds still fires the trigger.
	current := decode(t, h.do(t, http.MethodGet, target, "", nil))
	if current["token_id"] != tokenID || current["on_approval_required"] != "notify_and_wait" {
		t.Fatalf("trigger after the failed rotation = %v", current)
	}
	if got := h.vault.size(); got != vaultEntries {
		t.Fatalf("vault entries = %d, want %d (the unused credential is removed)", got, vaultEntries)
	}
	fired := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"go"}`,
		map[string]string{"Authorization": "Bearer " + secret})
	if fired.Code != http.StatusAccepted {
		t.Fatalf("fire with the kept secret: %d %s", fired.Code, fired.Body.String())
	}
}

func TestScheduleSaveWithAFailedApprovalSaveChangesNothing(t *testing.T) {
	h := newHarness(t)
	installApprovalTables(t, h.pool)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	target := fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID)
	if response := h.do(t, http.MethodPut, target,
		`{"cron":"0 3 * * *","active":true,"on_approval_required":"skip"}`, nil); response.Code != http.StatusOK {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}
	breakApprovalSettingsWrites(t, h.pool)
	response := h.do(t, http.MethodPut, target,
		`{"cron":"0 5 * * *","active":false,"on_approval_required":"fail"}`, nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("save with a failing approval save: %d %s, want 500", response.Code, response.Body.String())
	}
	current := h.readSchedule(t, versionID)
	if current["cron"] != "0 3 * * *" || current["active"] != true || current["on_approval_required"] != "skip" {
		t.Fatalf("schedule after the failed save = %v", current)
	}
}

func TestApprovalActionSaveWithoutTheTableIsSkipped(t *testing.T) {
	h := newHarness(t) // the tenant corpus only: no shared 0211 tables
	versionID := seedPipeline(t, h.pool, homeSchema, "Inbound", ownerUserID)
	response := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v2/pipeline_triggers/prompt_lib/%s/%d", homeProject, versionID),
		`{"on_approval_required":"skip"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mint without the approval tables: %d %s", response.Code, response.Body.String())
	}
	schedule := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID),
		`{"cron":"0 3 * * *","active":true,"on_approval_required":"skip"}`, nil)
	if schedule.Code != http.StatusOK {
		t.Fatalf("schedule without the approval tables: %d %s", schedule.Code, schedule.Body.String())
	}
}

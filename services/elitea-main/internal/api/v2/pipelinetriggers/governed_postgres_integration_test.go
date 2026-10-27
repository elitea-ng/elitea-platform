package pipelinetriggers_test

// Mandate-bound schedules and triggers (ADR-0039, issue 1122), against a real
// PostgreSQL. The mandate store is the runmandatetest double driving the real
// runmandate.Service; the security context the admission writes from the
// authority is covered by the repos integration test
// (execution_mandate_authority_postgres_integration_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const subjectUserID = int64(57)

// authorityStart is fakeStart that also records the run authority the
// admission context carried.
type authorityStart struct {
	*fakeStart
	authorities []securitycontext.RunAuthority
}

func (s *authorityStart) StartCurrentApplication(
	ctx context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	if authority, ok := securitycontext.RunAuthorityFrom(ctx); ok {
		s.mu.Lock()
		s.authorities = append(s.authorities, authority)
		s.mu.Unlock()
	}
	return s.fakeStart.StartCurrentApplication(ctx, request)
}

type governedHarness struct {
	*harness
	start    *authorityStart
	mandates *runmandatetest.Store
}

// newGovernedHarness is newHarness with WithGovernedRuns, over the shipped
// tenant 0158. The subject user holds the run permission in the home project.
func newGovernedHarness(t *testing.T) *governedHarness {
	t.Helper()
	h := newHarness(t)
	migration, err := platformmigrations.Files.ReadFile("tenant/0158_unattended_run_mandates.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{homeSchema, otherSchema} {
		if _, err := h.pool.Exec(context.Background(),
			fmt.Sprintf("SET search_path TO %s; %s", pgx.Identifier{schema}.Sanitize(), string(migration))); err != nil {
			t.Fatalf("apply 0158 to %s: %v", schema, err)
		}
	}
	mandates := runmandatetest.New()
	start := &authorityStart{fakeStart: h.start}
	h.handler = pipelinetriggers.NewPlatformHandler(
		h.pool, start, h.vault,
		multiUserPermissions{users: map[int64]bool{ownerUserID: true, subjectUserID: true}},
		h.recorder, nil, nil, nil,
		pipelinetriggers.WithGovernedRuns(mandates.Service()),
	)
	h.router = mountRoutes(h.handler)
	return &governedHarness{harness: h, start: start, mandates: mandates}
}

// multiUserPermissions grants the run permission to users in the home
// project.
type multiUserPermissions struct{ users map[int64]bool }

func (p multiUserPermissions) ResolvePermissions(
	_ context.Context, principal auth.User, _ string, projectID string,
) (auth.PermissionResolution, error) {
	id, _ := strconv.ParseInt(principal.UserID, 10, 64)
	if projectID != homeProject || !p.users[id] {
		return auth.PermissionResolution{}, nil
	}
	return auth.PermissionResolution{UserID: id, Permissions: []string{pipelinetriggers.RunPermission}}, nil
}

func (h *governedHarness) attachSchedule(t *testing.T, versionID int64, mandateID string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE p_1.pipeline_schedules SET mandate_id = $2 WHERE version_id = $1`, versionID, mandateID); err != nil {
		t.Fatal(err)
	}
}

func (h *governedHarness) scheduleMandate(t *testing.T, versionID int64) *string {
	t.Helper()
	var mandateID *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT mandate_id FROM p_1.pipeline_schedules WHERE version_id = $1`, versionID).Scan(&mandateID); err != nil {
		t.Fatal(err)
	}
	return mandateID
}

func governedNow() time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
}

func TestGovernedScheduleWithoutAMandateIsSkipped(t *testing.T) {
	h := newGovernedHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)

	result, err := h.handler.RunDueSchedules(context.Background(), governedNow())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.Dispatched != 0 || result.Skipped != 1 || h.start.count() != 0 {
		t.Fatalf("a schedule with no mandate ran: %+v starts=%d", result, h.start.count())
	}
	after := h.readSchedule(t, versionID)
	if after["last_result"] != "skipped_mandate_required" || after["last_run"] != nil ||
		!strings.HasPrefix(fmt.Sprint(after["last_result_detail"]), runmandate.CodeMandateRequired) {
		t.Fatalf("schedule after a refused fire = %v", after)
	}
	refusals := h.mandates.Refusals()
	if len(refusals) != 1 || refusals[0].Code != runmandate.CodeMandateRequired ||
		refusals[0].Target.Origin != runmandate.OriginPipelineSchedule {
		t.Fatalf("security audit refusals = %+v", refusals)
	}
	// No admission was attempted, so no admission row exists to read.
	var executions int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.pipeline_schedules WHERE last_execution_id IS NOT NULL`).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if executions != 0 {
		t.Fatal("a refused fire recorded an execution")
	}
}

func TestGovernedScheduleRunsUnderItsMandate(t *testing.T) {
	h := newGovernedHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)
	h.mandates.PutBinding("binding-1", 1, versionID)
	h.mandates.PutMandate("background", 1, mandate.KindDelegatedBackground, "binding-1", subjectUserID)
	h.attachSchedule(t, versionID, "background")

	result, err := h.handler.RunDueSchedules(context.Background(), governedNow())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.Dispatched != 1 || len(h.start.authorities) != 1 {
		t.Fatalf("result=%+v authorities=%+v", result, h.start.authorities)
	}
	if got := h.start.authorities[0]; got.MandateID != "background" || got.MandateEpoch != 2 || got.SubjectUserID != subjectUserID {
		t.Fatalf("authority = %+v", got)
	}
	request, _ := h.start.last()
	if request.ActorUserID != subjectUserID {
		t.Fatalf("ActorUserID = %d, want the mandate's subject %d", request.ActorUserID, subjectUserID)
	}
	if after := h.readSchedule(t, versionID); after["last_result"] != "dispatched" {
		t.Fatalf("last_result = %v", after["last_result"])
	}
}

func TestGovernedAutonomousScheduleHasNoSubject(t *testing.T) {
	h := newGovernedHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)
	h.mandates.PutBinding("binding-1", 1, versionID)
	h.mandates.PutMandate("auto", 1, mandate.KindAutonomous, "binding-1", 0)
	h.attachSchedule(t, versionID, "auto")

	if _, err := h.handler.RunDueSchedules(context.Background(), governedNow()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.start.authorities) != 1 || h.start.authorities[0].SubjectUserID != 0 {
		t.Fatalf("authorities = %+v", h.start.authorities)
	}
}

func TestGovernedScheduleWithARevokedOrExpiredMandateIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*mandate.Mandate)
	}{
		{"revoked", func(m *mandate.Mandate) { m.State = mandate.StateRevoked; m.RevocationEpoch = 3 }},
		{"expired", func(m *mandate.Mandate) { m.ExpiresAt = time.Now().Add(-time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGovernedHarness(t)
			versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
			h.saveSchedule(t, versionID, "* * * * *", true)
			h.mandates.PutBinding("binding-1", 1, versionID)
			h.mandates.PutMandate("auto", 1, mandate.KindAutonomous, "binding-1", 0)
			h.mandates.Update("auto", tc.change)
			h.attachSchedule(t, versionID, "auto")

			result, err := h.handler.RunDueSchedules(context.Background(), governedNow())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if result.Skipped != 1 || h.start.count() != 0 {
				t.Fatalf("result=%+v starts=%d", result, h.start.count())
			}
			if after := h.readSchedule(t, versionID); after["last_result"] != "skipped_mandate_not_usable" || after["last_run"] != nil {
				t.Fatalf("schedule = %v", after)
			}
			if refusals := h.mandates.Refusals(); len(refusals) != 1 || refusals[0].Code != runmandate.CodeMandateNotUsable {
				t.Fatalf("refusals = %+v", refusals)
			}
		})
	}
}

func TestGovernedCronEditKeepsTheMandateAndStaysInScope(t *testing.T) {
	h := newGovernedHarness(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "0 * * * *", true)
	h.mandates.PutBinding("binding-1", 1, versionID)
	h.mandates.PutMandate("auto", 1, mandate.KindAutonomous, "binding-1", 0,
		runmandate.CronScope("0 * * * *"), runmandate.CronScope("30 * * * *"))
	h.attachSchedule(t, versionID, "auto")

	// Inside the scope: saved, and the attachment survives the save.
	h.saveSchedule(t, versionID, "30 * * * *", true)
	if got := h.scheduleMandate(t, versionID); got == nil || *got != "auto" {
		t.Fatalf("mandate_id after a cron edit = %v", got)
	}

	// Beyond it: refused, and nothing changed.
	body, _ := json.Marshal(map[string]any{"cron": "15 * * * *", "active": true, "input": "scheduled"})
	response := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", homeProject, versionID), string(body), nil)
	if response.Code != http.StatusConflict || decode(t, response)["code"] != runmandate.CodeMandateScopeMismatch {
		t.Fatalf("out-of-scope save: status = %d, body = %s", response.Code, response.Body.String())
	}
	if after := h.readSchedule(t, versionID); after["cron"] != "30 * * * *" {
		t.Fatalf("a refused save changed the cron: %v", after["cron"])
	}
	if got := h.scheduleMandate(t, versionID); got == nil || *got != "auto" {
		t.Fatalf("mandate_id after a refused save = %v", got)
	}
}

func TestGovernedInboundTriggerNeedsAMandate(t *testing.T) {
	h := newGovernedHarness(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)
	headers := map[string]string{"Authorization": "Bearer " + secret}

	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"run it"}`, headers)
	if response.Code != http.StatusForbidden || decode(t, response)["code"] != runmandate.CodeMandateRequired {
		t.Fatalf("no mandate: status = %d, body = %s", response.Code, response.Body.String())
	}
	if h.start.count() != 0 {
		t.Fatal("a trigger with no mandate started a run")
	}
	if refusals := h.mandates.Refusals(); len(refusals) != 1 || refusals[0].Target.Origin != runmandate.OriginPipelineTrigger {
		t.Fatalf("refusals = %+v", refusals)
	}

	h.mandates.PutBinding("binding-1", 1, versionID)
	h.mandates.PutMandate("auto", 1, mandate.KindAutonomous, "binding-1", 0)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE p_1.pipeline_triggers SET mandate_id = 'auto' WHERE version_id = $1`, versionID); err != nil {
		t.Fatal(err)
	}
	response = h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID), `{"input":"run it"}`, headers)
	if response.Code != http.StatusAccepted {
		t.Fatalf("with a mandate: status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(h.start.authorities) != 1 {
		t.Fatalf("authorities = %+v", h.start.authorities)
	}
	got := h.start.authorities[0]
	if got.MandateID != "auto" || got.Caller.CallingApplicationID == "" ||
		!strings.HasPrefix(got.Caller.AuthenticationSource, "pipeline_trigger.") {
		t.Fatalf("authority = %+v", got)
	}
}

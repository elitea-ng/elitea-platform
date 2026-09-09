package pipelinetriggers_test

// Acceptance for the WRITE half of pipeline.run.succeeded/failed: admit()
// (run.go) records a public.pipeline_runs row through RunTracker in the SAME
// request that admits the run — the row a LATER execution.AfterSettleHook
// (internal/application/pipelineruns.NewSettlementHook) looks up once that
// run's outcome settles. See that package's doc comment for the full story;
// this file proves only the WRITE, against a real inbound trigger POST and a
// real database, reusing this package's own harness fixtures (mintTrigger,
// seedPipeline, inboundTarget).

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// fakeRunTracker is an in-memory pipelinetriggers.RunTracker, local to this
// file so it can assert exactly what admit() recorded without a database
// round trip of its own.
type fakeRunTracker struct {
	mu   sync.Mutex
	runs []pipelineruns.Run
}

func (f *fakeRunTracker) RecordRunStart(_ context.Context, run pipelineruns.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakeRunTracker) all() []pipelineruns.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pipelineruns.Run(nil), f.runs...)
}

// newHarnessWithRunTracker is newHarness (this package's own, in
// pipelinetriggers_postgres_integration_test.go) plus a fakeRunTracker
// wired via WithRunTracker — everything else identical, including the
// permission grant to ownerUserID in homeProject only.
func newHarnessWithRunTracker(t *testing.T) (*harness, *fakeRunTracker) {
	t.Helper()
	pool := newPool(t)
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID:       "execution-tracked-1",
		CommandID:         "command-tracked-1",
		ResponseMessageID: "22222222-2222-4222-8222-222222222222",
		Created:           true,
	}}
	vault := newMemoryVault()
	recorder := &recordingRecorder{}
	tracker := &fakeRunTracker{}
	handler := pipelinetriggers.NewPlatformHandler(
		pool, start, vault,
		fixedPermissions{
			userID:      ownerUserID,
			projectID:   homeProject,
			permissions: []string{pipelinetriggers.RunPermission},
		},
		recorder, nil, nil, tracker,
	)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{
				ID:     strconv.FormatInt(ownerUserID, 10),
				UserID: strconv.FormatInt(ownerUserID, 10),
				Email:  "owner@example.com",
			}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/api/v2/pipeline_triggers/prompt_lib/{projectID}/{versionID}", handler.CreateOrRotateTrigger)
	router.Put("/api/v2/pipeline_schedules/prompt_lib/{projectID}/{versionID}", handler.SaveSchedule)
	router.Post(pipelinetriggers.InboundPath, handler.Trigger)
	return &harness{pool: pool, start: start, vault: vault, recorder: recorder, handler: handler, router: router}, tracker
}

// TestInboundTriggerRecordsAPipelineRunTrackerRow proves admit() writes the
// tracker row, with the exact application/version/conversation identity the
// admitted run itself carries, in the SAME request as the 202.
func TestInboundTriggerRecordsAPipelineRunTrackerRow(t *testing.T) {
	h, tracker := newHarnessWithRunTracker(t)
	versionID, tokenID, secret := h.mintTrigger(t, homeProject, homeSchema, "Nightly report", ownerUserID)

	response := h.do(t, http.MethodPost, inboundTarget(homeProject, tokenID),
		`{"input":"run it"}`, map[string]string{"Authorization": "Bearer " + secret})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	executionID, _ := body["execution_id"].(string)
	conversationUUID, _ := body["conversation_id"].(string)
	if executionID == "" || conversationUUID == "" {
		t.Fatalf("trigger response carries no execution/conversation identity: %v", body)
	}

	runs := tracker.all()
	if len(runs) != 1 {
		t.Fatalf("RecordRunStart called %d times, want 1", len(runs))
	}
	got := runs[0]
	if got.ExecutionID != executionID {
		t.Errorf("ExecutionID = %q, want %q", got.ExecutionID, executionID)
	}
	if got.ProjectID != homeProject {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, homeProject)
	}
	if got.VersionID != versionID {
		t.Errorf("VersionID = %d, want %d", got.VersionID, versionID)
	}
	if got.ConversationUUID != conversationUUID {
		t.Errorf("ConversationUUID = %q, want %q", got.ConversationUUID, conversationUUID)
	}
	if got.Origin != pipelinetriggers.OriginWebhook {
		t.Errorf("Origin = %q, want %q", got.Origin, pipelinetriggers.OriginWebhook)
	}
}

// TestScheduleFireRecordsAPipelineRunTrackerRowWithScheduleOrigin proves the
// SAME write happens from the schedule tick's admission, with Origin
// distinguishing it from an inbound trigger.
func TestScheduleFireRecordsAPipelineRunTrackerRowWithScheduleOrigin(t *testing.T) {
	h, tracker := newHarnessWithRunTracker(t)
	versionID := seedPipeline(t, h.pool, homeSchema, "Nightly report", ownerUserID)
	h.saveSchedule(t, versionID, "* * * * *", true)

	result, err := h.handler.RunDueSchedules(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("RunDueSchedules: %v", err)
	}
	if result.Dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1: %+v", result.Dispatched, result)
	}

	runs := tracker.all()
	if len(runs) != 1 {
		t.Fatalf("RecordRunStart called %d times, want 1", len(runs))
	}
	if runs[0].Origin != pipelinetriggers.OriginSchedule {
		t.Errorf("Origin = %q, want %q", runs[0].Origin, pipelinetriggers.OriginSchedule)
	}
	if runs[0].VersionID != versionID {
		t.Errorf("VersionID = %d, want %d", runs[0].VersionID, versionID)
	}
}

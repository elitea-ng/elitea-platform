package eliteacore_test

// ADR-0040 decision 3 on the direct publish route: a governed agent is
// refused before the publish checks make a model call, and the registry
// row's lock is the first statement of the publish transaction (the lock
// order the change request's final approval uses).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// fakePublishGovernor answers CheckDirectPublish with refuse and records
// the transaction calls.
type fakePublishGovernor struct {
	refuse        error
	checked       int64
	calls         []string
	xidAtLock     bool
	lockStatement error
}

func (f *fakePublishGovernor) CheckDirectPublish(_ context.Context, _, applicationID int64) error {
	f.checked = applicationID
	f.calls = append(f.calls, "check")
	return f.refuse
}

func (f *fakePublishGovernor) LockRegistryTx(ctx context.Context, tx pgx.Tx, _, _ int64) error {
	f.calls = append(f.calls, "lock")
	// No transaction id yet: nothing was written or row-locked before.
	f.lockStatement = tx.QueryRow(ctx, `SELECT pg_current_xact_id_if_assigned() IS NOT NULL`).Scan(&f.xidAtLock)
	return nil
}

func (f *fakePublishGovernor) RecordDirectPublishTx(context.Context, pgx.Tx, agentlifecycle.DirectPublish) error {
	f.calls = append(f.calls, "record")
	return nil
}

func publishThroughRoute(t *testing.T, handler *eliteacore.Handler) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/elitea_core/publish/prompt_lib/%d/91", publishAIProjectID),
		strings.NewReader(`{"version_name":"release-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: fmt.Sprintf("%d", publishAICallerID)}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// TestPublishOfGovernedAgentMakesNoModelCall: change_request_required comes
// before the validation gate's model call.
func TestPublishOfGovernedAgentMakesNoModelCall(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)
	model := &fakePublishModel{answer: `{"findings":[]}`}
	governor := &fakePublishGovernor{refuse: agentlifecycle.Refuse(agentlifecycle.ChangeRequiredCode, "needs approval")}
	recorder := publishThroughRoute(t, eliteacore.NewHandler(pool,
		eliteacore.WithPublishAIValidation(model), eliteacore.WithPublishGovernor(governor)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), agentlifecycle.ChangeRequiredCode) {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if model.calls != 0 {
		t.Fatalf("the model was called %d times before the governance refusal", model.calls)
	}
	if governor.checked != 81 {
		t.Fatalf("governance checked application %d, want 81", governor.checked)
	}
}

// TestDirectPublishLocksTheRegistryFirst: the registry lock is taken before
// any write or row lock of the publish transaction.
func TestDirectPublishLocksTheRegistryFirst(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)
	governor := &fakePublishGovernor{}
	recorder := publishThroughRoute(t, eliteacore.NewHandler(pool,
		eliteacore.WithPublishAIValidation(&fakePublishModel{answer: `{"findings":[]}`}),
		eliteacore.WithPublishGovernor(governor)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if strings.Join(governor.calls, ",") != "check,lock,record" {
		t.Fatalf("governor calls = %v, want check, lock, record", governor.calls)
	}
	if governor.lockStatement != nil || governor.xidAtLock {
		t.Fatalf("the registry lock was not the first statement: xid assigned %v, %v", governor.xidAtLock, governor.lockStatement)
	}
}

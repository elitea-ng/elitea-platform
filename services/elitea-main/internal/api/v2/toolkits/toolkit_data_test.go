package toolkits_test

// Deleting a toolkit whose application engine holds data for it (#1244): the
// engine goes first, a refusal leaves the row, and the client is told which of
// the two it was.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// deleteRepo is a repository that describes one toolkit and records the row
// deletes it was asked for.
type deleteRepo struct {
	mockRepo
	toolkitType string
	getErr      error
	deleted     []string
}

func (r *deleteRepo) GetToolkit(_ context.Context, _, toolkitID string) (map[string]any, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return map[string]any{"id": toolkitID, "type": r.toolkitType}, nil
}

func (r *deleteRepo) DeleteToolkit(_ context.Context, _, toolkitID string) error {
	r.deleted = append(r.deleted, toolkitID)
	return nil
}

type recordingRemover struct {
	calls []struct {
		project, toolkit int64
		toolkitType      string
	}
	err error
}

func (f *recordingRemover) DeleteToolkitData(_ context.Context, project, toolkit int64, toolkitType string) error {
	f.calls = append(f.calls, struct {
		project, toolkit int64
		toolkitType      string
	}{project, toolkit, toolkitType})
	return f.err
}

func deleteRequest(t *testing.T, handler *toolkits.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Delete)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
	return response
}

func TestDeleteAsksTheEngineFirstThenRemovesTheRow(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{toolkitType: "inventory_inventory"}
	remover := &recordingRemover{}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitDataRemover(remover))

	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(remover.calls) != 1 || remover.calls[0].project != 7 || remover.calls[0].toolkit != 70 ||
		remover.calls[0].toolkitType != "inventory_inventory" {
		t.Fatalf("remover calls = %+v", remover.calls)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "70" {
		t.Fatalf("row deletes = %v, want [70]", repo.deleted)
	}
}

func TestABusyEngineAnswers409AndLeavesTheRow(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{toolkitType: "inventory_inventory"}
	remover := &recordingRemover{err: toolkits.ErrToolkitDataBusy}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitDataRemover(remover))

	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("the row was deleted under a refusal: %v", repo.deleted)
	}
	if !bodyContains(response.Body.String(), "retry") {
		t.Errorf("the client is not told to retry: %s", response.Body.String())
	}
}

func TestAnUnreachableEngineAnswers503LeavesTheRowAndHidesTheCause(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{toolkitType: "inventory_inventory"}
	remover := &recordingRemover{err: errors.New("rpc error: connection refused to 10.0.7.3:9443")}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitDataRemover(remover))

	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("the row was deleted under a failure: %v", repo.deleted)
	}
	if bodyContains(response.Body.String(), "10.0.7.3") {
		t.Errorf("the response leaks the engine address: %s", response.Body.String())
	}
}

func TestWithoutARemoverDeleteIsTheRowAlone(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{toolkitType: "inventory_inventory"}
	handler := toolkits.NewHandlerWithRepo(repo)
	if response := deleteRequest(t, handler, "/tool/prompt_lib/7/70"); response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
	if len(repo.deleted) != 1 {
		t.Fatalf("row deletes = %v", repo.deleted)
	}
}

// A toolkit the repository cannot describe (already gone) is the row delete's
// to answer, as before: the engine is not asked about a toolkit nobody can
// name the type of, and the remover's wiring does not change the 204.
func TestAToolkitTheRepositoryCannotDescribeSkipsTheEngine(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{getErr: errors.New("no rows")}
	remover := &recordingRemover{err: errors.New("must not be called")}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitDataRemover(remover))
	if response := deleteRequest(t, handler, "/tool/prompt_lib/7/70"); response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(remover.calls) != 0 {
		t.Fatalf("the engine was asked: %+v", remover.calls)
	}
	if len(repo.deleted) != 1 {
		t.Fatalf("row deletes = %v", repo.deleted)
	}
}

func TestANonNumericIDSkipsTheEngine(t *testing.T) {
	t.Parallel()
	repo := &deleteRepo{toolkitType: "inventory_inventory"}
	remover := &recordingRemover{err: errors.New("must not be called")}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitDataRemover(remover))
	deleteRequest(t, handler, "/tool/prompt_lib/7/abc")
	if len(remover.calls) != 0 {
		t.Fatalf("the engine was asked for a non-numeric toolkit id: %+v", remover.calls)
	}
}

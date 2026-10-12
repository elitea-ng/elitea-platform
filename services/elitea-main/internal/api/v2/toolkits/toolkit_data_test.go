package toolkits_test

// Deleting a toolkit whose application engine holds data for it (#1244): the
// row goes first, in one transaction with the journal row that records the
// engine-side deletion owed, and the request never depends on the engine.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// rowFirstRepo records which delete the handler asked for. Its row-first
// delete plays the repository's part of the transaction: it hands the journal
// the toolkit's type, as `DELETE ... RETURNING type` would.
type rowFirstRepo struct {
	mockRepo
	toolkitType string
	rowFirst    []string
	rowOnly     []string
	// rowFirstErr is what the row-first delete returns (a database failure).
	rowFirstErr error
}

func (r *rowFirstRepo) DeleteToolkit(_ context.Context, _, toolkitID string) error {
	r.rowOnly = append(r.rowOnly, toolkitID)
	return nil
}

func (r *rowFirstRepo) DeleteToolkitRowFirst(ctx context.Context, projectID, toolkitID string, journal toolkits.ToolkitCleanupJournal) error {
	r.rowFirst = append(r.rowFirst, toolkitID)
	if r.rowFirstErr != nil {
		return r.rowFirstErr
	}
	_, err := journal.Enqueue(ctx, nil, 7, 70, r.toolkitType)
	return err
}

type recordingJournal struct {
	types []string
	err   error
}

func (j *recordingJournal) Enqueue(_ context.Context, _ pgx.Tx, _, _ int64, toolkitType string) (string, error) {
	j.types = append(j.types, toolkitType)
	return "inventory", j.err
}

func deleteRequest(t *testing.T, handler *toolkits.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Delete)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
	return response
}

// With a journal wired, Delete is the row-first transaction: the row delete
// and the journal write together, and the 204 follows the commit. The engine
// is not in the request at all.
func TestDeleteIsRowFirstWithTheJournalInTheSameTransaction(t *testing.T) {
	t.Parallel()
	repo := &rowFirstRepo{toolkitType: "inventory_inventory"}
	journal := &recordingJournal{}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitCleanupJournal(journal))

	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(repo.rowFirst) != 1 || repo.rowFirst[0] != "70" {
		t.Fatalf("row-first deletes = %v, want [70]", repo.rowFirst)
	}
	if len(repo.rowOnly) != 0 {
		t.Fatalf("the row-only delete ran too: %v", repo.rowOnly)
	}
	if len(journal.types) != 1 || journal.types[0] != "inventory_inventory" {
		t.Fatalf("journal saw %v", journal.types)
	}
}

// A journal write that fails is a transaction that fails: 500, the cause
// hidden, and (the repository's contract) nothing deleted.
func TestAJournalFailureFailsTheDeleteAndHidesTheCause(t *testing.T) {
	t.Parallel()
	repo := &rowFirstRepo{toolkitType: "inventory_inventory"}
	journal := &recordingJournal{err: errors.New("write cleanup journal: dial tcp 10.0.7.3:5432: connection refused")}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitCleanupJournal(journal))

	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bodyContains(response.Body.String(), "10.0.7.3") {
		t.Errorf("the response leaks the database address: %s", response.Body.String())
	}
}

func TestADatabaseFailureOfTheRowFirstDeleteIs500(t *testing.T) {
	t.Parallel()
	repo := &rowFirstRepo{toolkitType: "inventory_inventory", rowFirstErr: errors.New("dial tcp 10.0.7.3:5432: connection refused")}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitCleanupJournal(&recordingJournal{}))
	response := deleteRequest(t, handler, "/tool/prompt_lib/7/70")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bodyContains(response.Body.String(), "10.0.7.3") {
		t.Errorf("the response leaks the database address: %s", response.Body.String())
	}
}

// Without a journal the delete is the row alone, as every deployment did
// before the engines served a platform service.
func TestWithoutAJournalDeleteIsTheRowAlone(t *testing.T) {
	t.Parallel()
	repo := &rowFirstRepo{toolkitType: "inventory_inventory"}
	handler := toolkits.NewHandlerWithRepo(repo)
	if response := deleteRequest(t, handler, "/tool/prompt_lib/7/70"); response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
	if len(repo.rowOnly) != 1 || len(repo.rowFirst) != 0 {
		t.Fatalf("row-only=%v row-first=%v, want the row-only delete", repo.rowOnly, repo.rowFirst)
	}
}

// A nil journal option is no journal: the option is declared with the
// interface type at the composition root, and a nil there must not turn every
// delete into a nil-pointer panic.
func TestANilJournalOptionIsIgnored(t *testing.T) {
	t.Parallel()
	repo := &rowFirstRepo{}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithToolkitCleanupJournal(nil))
	if response := deleteRequest(t, handler, "/tool/prompt_lib/7/70"); response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
	if len(repo.rowOnly) != 1 {
		t.Fatalf("row-only deletes = %v", repo.rowOnly)
	}
}

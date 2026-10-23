package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// dbFaultText is the shape a pgx connect failure takes after the repository
// wraps it. It names the database host, the database user and the SQLSTATE
// code.
const dbFaultText = "list artifact buckets: failed to connect to host=db.internal " +
	"user=elitea database=elitea: server error (SQLSTATE 08006)"

// failingListRepo answers ListBuckets with a database fault. Every other
// method keeps the in-memory behaviour of fakeRepo.
type failingListRepo struct {
	artifacts.Repository
	err error
}

func (r *failingListRepo) ListBuckets(context.Context, int64) ([]repos.BucketRow, error) {
	return nil, r.err
}

// TestListBucketsHidesTheDatabaseCauseFromTheCaller pins the error contract
// for a 500.
//
// DEFECT: the artifact handlers concatenated the raw err.Error() into the
// response envelope, so GET /api/v2/artifacts/buckets/{projectID} answered
// a database fault with
// {"error":{"code":"Internal","message":"list buckets: failed to connect to
// host=... SQLSTATE 08006"}}. That gives a project member the database host,
// the database user and the driver state. AGENTS.md states the rule: log the
// internal cause, never return a raw err.Error() across a trust boundary.
//
// The package also had no log statement at all, so the response body was the
// only record of the cause. The test therefore checks both halves: the cause
// must leave the response AND must arrive in the log.
func TestListBucketsHidesTheDatabaseCauseFromTheCaller(t *testing.T) {
	var logs bytes.Buffer
	repo := &failingListRepo{Repository: newFakeRepo(), err: errors.New(dbFaultText)}
	h := artifacts.NewHandler(repo, newFakeStore()).
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	req := httptest.NewRequest(http.MethodGet, "/buckets/1", nil)
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	for _, secret := range []string{"db.internal", "user=elitea", "SQLSTATE"} {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"code":"Internal"`) {
		t.Errorf("expected the typed Internal code, got %s", body)
	}

	if !strings.Contains(logs.String(), "SQLSTATE 08006") {
		t.Errorf("expected the cause in the log, got %q", logs.String())
	}
}

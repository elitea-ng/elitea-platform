package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type reviewSource struct {
	queue []repos.DelegationProjectQueue
	scan  repos.DelegationMigrationReport
	err   error
}

func (s reviewSource) ReviewQueue(context.Context) ([]repos.DelegationProjectQueue, error) {
	return s.queue, s.err
}

func (s reviewSource) ScanAll(context.Context) (repos.DelegationMigrationReport, error) {
	return s.scan, s.err
}

func TestDelegationReviewTotalsTheProjectsAndSaysWhatTheModeIs(t *testing.T) {
	h := &Handler{delegationReview: reviewSource{queue: []repos.DelegationProjectQueue{
		{ProjectID: 1, ProjectName: "Alpha", Pending: 3, Legacy: 2, Stale: 1, Callers: []repos.DelegationQueueCaller{{ApplicationID: 9, Name: "Parent", Pending: 3, Stale: 1}}},
		{ProjectID: 2, ProjectName: "Beta", Pending: 1, Legacy: 1},
	}}}
	recorder := httptest.NewRecorder()
	h.DelegationReviewList(recorder, httptest.NewRequest(http.MethodGet, "/admin/delegation_review", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body)
	}
	var review DelegationReview
	if err := json.Unmarshal(recorder.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.Pending != 4 || review.Legacy != 3 || review.Stale != 1 || len(review.Projects) != 2 {
		t.Fatalf("totals: %+v", review)
	}
	// No pool: the defaults. An installation with no saved mode enforces, the
	// principal bound is on, and the report window is the recommended 14 days.
	if review.Mode != "enforce" || review.ModeStored || !review.PrincipalBound || review.ReportWindowDays != 14 {
		t.Fatalf("mode: %+v", review)
	}
	if review.Projects[0].Callers[0].Name != "Parent" || review.Projects[1].Callers == nil {
		t.Fatalf("callers: %+v", review.Projects)
	}
}

func TestDelegationReviewWithoutASourceIs503AndAFailureIs500(t *testing.T) {
	for name, test := range map[string]struct {
		h    *Handler
		want int
	}{
		"no source":     {&Handler{}, http.StatusServiceUnavailable},
		"unreadable db": {&Handler{delegationReview: reviewSource{err: errors.New("boom")}}, http.StatusInternalServerError},
	} {
		recorder := httptest.NewRecorder()
		test.h.DelegationReviewList(recorder, httptest.NewRequest(http.MethodGet, "/admin/delegation_review", nil))
		if recorder.Code != test.want {
			t.Errorf("%s: list status = %d, want %d", name, recorder.Code, test.want)
		}
		recorder = httptest.NewRecorder()
		test.h.DelegationReviewScan(recorder, httptest.NewRequest(http.MethodPost, "/admin/delegation_review/scan", nil))
		if recorder.Code != test.want {
			t.Errorf("%s: scan status = %d, want %d", name, recorder.Code, test.want)
		}
	}
}

func TestDelegationReviewScanAnswersWhatItDid(t *testing.T) {
	h := &Handler{delegationReview: reviewSource{scan: repos.DelegationMigrationReport{Projects: 3, Migrated: 3, Edges: 7, Created: 2, Notified: 4}}}
	recorder := httptest.NewRecorder()
	h.DelegationReviewScan(recorder, httptest.NewRequest(http.MethodPost, "/admin/delegation_review/scan", nil))
	var result DelegationScanResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result != (DelegationScanResult{Projects: 3, Scanned: 3, Edges: 7, Created: 2, Notified: 4}) {
		t.Fatalf("result = %+v", result)
	}
}

// The Delegation settings carry the pending count and the report-window hint.
func TestTheDelegationSettingsStatusCarriesThePendingCountAndTheWindow(t *testing.T) {
	h := &Handler{delegationReview: reviewSource{queue: []repos.DelegationProjectQueue{{ProjectID: 1, Pending: 5, Legacy: 5}}}}
	status, err := h.delegationReviewStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status["pending"] != 5 || status["legacy"] != 5 || status["report_window_days"] != 14 || status["mode"] != "enforce" {
		t.Fatalf("status = %v", status)
	}
}

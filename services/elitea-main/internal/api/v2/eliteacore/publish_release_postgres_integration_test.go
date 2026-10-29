package eliteacore_test

// Real-PostgreSQL coverage of the release a publish records and an
// unpublish withdraws (ADR-0040 decision 2, issue #1492), through the
// production handler and the full migration chain.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestPublishRecordsAReleaseAndUnpublishWithdrawsIt(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedPublishCopyFixture(t, pool)
	store, err := repos.NewAgentReleases(pool)
	if err != nil {
		t.Fatal(err)
	}
	handler := eliteacore.NewHandler(pool, eliteacore.WithReleases(store))
	router := publishCopyRouter(handler)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", handler.Unpublish)

	recorder := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name":     "v-release",
		"validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var published struct {
		PublicVersionID string `json:"public_version_id"`
		ReleaseID       string `json:"release_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &published); err != nil || published.ReleaseID == "" {
		t.Fatalf("publish response %s (%v): no release id", recorder.Body, err)
	}
	releaseID, _ := strconv.ParseInt(published.ReleaseID, 10, 64)
	versionID, _ := strconv.ParseInt(published.PublicVersionID, 10, 64)

	releases, err := store.ListReleases(context.Background(), 1, int64(fixture.appID))
	if err != nil || len(releases) != 1 || releases[0].ID != releaseID || !releases[0].IsHead ||
		releases[0].ApplicationVersionID != versionID || releases[0].Name != "v-release" {
		t.Fatalf("releases %+v (%v)", releases, err)
	}
	detail, err := store.GetRelease(context.Background(), 1, int64(fixture.appID), releaseID)
	if err != nil || len(detail.Release.ToolkitPins) != 1 || detail.Release.ToolkitPins[0].ToolkitID != int64(fixture.toolID) {
		t.Fatalf("release pins %+v (%v)", detail.Release.ToolkitPins, err)
	}

	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/elitea_core/unpublish/prompt_lib/1/%d", versionID), strings.NewReader(`{}`))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "1", UserID: "1"}))
	unpublished := httptest.NewRecorder()
	router.ServeHTTP(unpublished, request)
	if unpublished.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d, body = %s", unpublished.Code, unpublished.Body.String())
	}
	releases, err = store.ListReleases(context.Background(), 1, int64(fixture.appID))
	if err != nil || len(releases) != 1 || releases[0].State != "withdrawn" || releases[0].IsHead || releases[0].WithdrawnBy != 1 {
		t.Fatalf("after the unpublish %+v (%v)", releases, err)
	}
}

// failingReleaseRecorder is the real store whose record or withdrawal
// fails, the way a database fault would, after doing nothing.
type failingReleaseRecorder struct {
	*repos.AgentReleases
	failRecord, failWithdraw bool
}

func (f *failingReleaseRecorder) RecordPublishedTx(ctx context.Context, tx pgx.Tx, projectID, applicationID, versionID, sourceVersionID int64, name string, actorID int64) (int64, error) {
	if f.failRecord {
		return 0, errors.New("injected: record the release")
	}
	return f.AgentReleases.RecordPublishedTx(ctx, tx, projectID, applicationID, versionID, sourceVersionID, name, actorID)
}

func (f *failingReleaseRecorder) WithdrawVersionsTx(ctx context.Context, tx pgx.Tx, projectID int64, versionIDs []int64, actorID int64) error {
	if f.failWithdraw {
		return errors.New("injected: withdraw the releases")
	}
	return f.AgentReleases.WithdrawVersionsTx(ctx, tx, projectID, versionIDs, actorID)
}

func releaseTestUnpublishDo(t *testing.T, router chi.Router, versionID int64) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/elitea_core/unpublish/prompt_lib/1/%d", versionID), strings.NewReader(`{}`))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "1", UserID: "1"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// A publish whose release cannot be recorded fails whole: 500, no
// published row, no release, and a retry publishes with its release. An
// unpublish whose withdrawal fails fails whole too: the version stays
// published with its release at the head, and a retry withdraws it (it
// used to answer 409 "version is not published" for good).
func TestPublishAndUnpublishRecordTheReleaseAtomically(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedPublishCopyFixture(t, pool)
	store, err := repos.NewAgentReleases(pool)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &failingReleaseRecorder{AgentReleases: store, failRecord: true}
	handler := eliteacore.NewHandler(pool, eliteacore.WithReleases(recorder))
	router := publishCopyRouter(handler)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", handler.Unpublish)
	ctx := context.Background()
	publishedRows := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND status = 'published'`, fixture.appID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	failed := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name": "v-atomic", "validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("publish with a failing release: status %d, body %s", failed.Code, failed.Body)
	}
	if got := publishedRows(); got != 0 {
		t.Fatalf("a publish whose release failed left %d published rows", got)
	}
	if releases, err := store.ListReleases(ctx, 1, int64(fixture.appID)); err != nil || len(releases) != 0 {
		t.Fatalf("releases after the failed publish: %+v (%v)", releases, err)
	}

	recorder.failRecord = false
	ok := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name": "v-atomic", "validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("publish retry: status %d, body %s", ok.Code, ok.Body)
	}
	var published struct {
		PublicVersionID string `json:"public_version_id"`
		ReleaseID       string `json:"release_id"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &published); err != nil || published.ReleaseID == "" {
		t.Fatalf("publish retry response %s (%v)", ok.Body, err)
	}
	versionID, _ := strconv.ParseInt(published.PublicVersionID, 10, 64)

	recorder.failWithdraw = true
	if got := releaseTestUnpublishDo(t, router, versionID); got.Code != http.StatusInternalServerError {
		t.Fatalf("unpublish with a failing withdrawal: status %d, body %s", got.Code, got.Body)
	}
	if got := publishedRows(); got != 1 {
		t.Fatalf("a failed unpublish left %d published rows, want the version still published", got)
	}
	releases, err := store.ListReleases(ctx, 1, int64(fixture.appID))
	if err != nil || len(releases) != 1 || releases[0].State != "published" || !releases[0].IsHead {
		t.Fatalf("releases after the failed unpublish: %+v (%v)", releases, err)
	}

	recorder.failWithdraw = false
	if got := releaseTestUnpublishDo(t, router, versionID); got.Code != http.StatusOK {
		t.Fatalf("unpublish retry: status %d, body %s", got.Code, got.Body)
	}
	releases, err = store.ListReleases(ctx, 1, int64(fixture.appID))
	if err != nil || len(releases) != 1 || releases[0].State != "withdrawn" || releases[0].IsHead {
		t.Fatalf("releases after the unpublish retry: %+v (%v)", releases, err)
	}
	if got := publishedRows(); got != 0 {
		t.Fatalf("the unpublish retry left %d published rows", got)
	}
}

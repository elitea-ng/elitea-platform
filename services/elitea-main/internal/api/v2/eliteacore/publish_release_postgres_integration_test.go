package eliteacore_test

// Real-PostgreSQL coverage of the release a publish records and an
// unpublish withdraws (ADR-0040 decision 2, issue #1492), through the
// production handler and the full migration chain.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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

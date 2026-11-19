package microsoftoauthapps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

type fakeSource struct {
	offers []microsoftapps.Offer
	err    error
	custom bool
	asked  struct {
		credentialType string
		projectID      int64
	}
}

func (f *fakeSource) Offers(_ context.Context, credentialType string, projectID int64) ([]microsoftapps.Offer, error) {
	f.asked.credentialType, f.asked.projectID = credentialType, projectID
	return f.offers, f.err
}

func (f *fakeSource) CustomAppsAllowed(context.Context) bool { return f.custom }

func call(t *testing.T, source Source, target string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Get(Path, New(source).List)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func TestAnEmptyRegistryOffersNothingAndKeepsCustomApp(t *testing.T) {
	got := call(t, &fakeSource{custom: true}, "/microsoft_oauth_apps/7?credential_type=outlook")
	var body struct {
		Apps             []offerView `json:"apps"`
		CustomAppAllowed bool        `json:"custom_app_allowed"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || got.Code != http.StatusOK {
		t.Fatalf("%d %s %v", got.Code, got.Body, err)
	}
	if body.Apps == nil || len(body.Apps) != 0 || !body.CustomAppAllowed {
		t.Errorf("empty registry answered %+v", body)
	}
	if got.Header().Get("Cache-Control") != "no-store" {
		t.Error("the offers depend on the caller's project and must not be cached")
	}
}

func TestOffersCarryNoClientIdAuthorityOrSecret(t *testing.T) {
	source := &fakeSource{custom: false, offers: []microsoftapps.Offer{
		{ID: "a1", Name: "Corp", Description: "Company app", AutoRefreshToken: true, Ceiling: []string{"Mail.Read"}},
		{ID: "a2", Name: "CRM"},
	}}
	got := call(t, source, "/microsoft_oauth_apps/7?credential_type=dataverse")
	if source.asked.credentialType != "dataverse" || source.asked.projectID != 7 {
		t.Errorf("asked %+v", source.asked)
	}
	text := got.Body.String()
	for _, leak := range []string{"client_id", "authority", "secret", "tenant", "login.microsoftonline.com"} {
		if strings.Contains(text, leak) {
			t.Errorf("the offers mention %q: %s", leak, text)
		}
	}
	var body struct {
		Apps             []offerView `json:"apps"`
		CustomAppAllowed bool        `json:"custom_app_allowed"`
	}
	_ = json.Unmarshal(got.Body.Bytes(), &body)
	if len(body.Apps) != 2 || body.CustomAppAllowed || body.Apps[1].ScopeCeiling == nil {
		t.Errorf("body = %+v", body)
	}
}

func TestBadInputAndFailures(t *testing.T) {
	for target, want := range map[string]int{
		"/microsoft_oauth_apps/7":                           http.StatusBadRequest,
		"/microsoft_oauth_apps/7?credential_type=gmail":     http.StatusBadRequest,
		"/microsoft_oauth_apps/abc?credential_type=outlook": http.StatusBadRequest,
		"/microsoft_oauth_apps/0?credential_type=outlook":   http.StatusBadRequest,
	} {
		if got := call(t, &fakeSource{}, target); got.Code != want {
			t.Errorf("%s = %d, want %d", target, got.Code, want)
		}
	}
	failed := call(t, &fakeSource{err: errors.New("pq: password authentication failed for user admin")}, "/microsoft_oauth_apps/7?credential_type=outlook")
	if failed.Code != http.StatusServiceUnavailable || strings.Contains(failed.Body.String(), "password") {
		t.Errorf("a store failure = %d %s", failed.Code, failed.Body)
	}
	var none Source
	if got := call(t, none, "/microsoft_oauth_apps/7?credential_type=outlook"); got.Code != http.StatusServiceUnavailable {
		t.Errorf("no registry = %d", got.Code)
	}
}

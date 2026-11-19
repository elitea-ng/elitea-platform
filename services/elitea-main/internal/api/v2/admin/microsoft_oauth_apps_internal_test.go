package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

const (
	msTestClient = "7f8d0c1e-0000-4000-8000-000000000001"
	msTestSecret = "s3cr3t~value.from.Entra"
)

// msStore is an in-memory registry that keeps the secret it is given in a field
// the views can never reach, so a test can prove the secret is not in any answer.
type msStore struct {
	apps     map[string]microsoftapps.App
	secrets  map[string]string
	inputs   []microsoftapps.Input
	err      error
	custom   bool
	disabled []string
}

func newMSStore() *msStore {
	return &msStore{apps: map[string]microsoftapps.App{}, secrets: map[string]string{}, custom: true}
}

func (s *msStore) List(context.Context) ([]microsoftapps.App, error) {
	out := []microsoftapps.App{}
	for _, app := range s.apps {
		out = append(out, app)
	}
	return out, s.err
}

func (s *msStore) Get(_ context.Context, id string) (microsoftapps.App, error) {
	if app, ok := s.apps[id]; ok {
		return app, nil
	}
	return microsoftapps.App{}, microsoftapps.ErrNotFound
}

func (s *msStore) save(id string, in microsoftapps.Input) microsoftapps.App {
	in, _ = in.Normalize()
	s.inputs = append(s.inputs, in)
	app := microsoftapps.App{
		ID: id, Name: in.Name, Description: in.Description, ClientID: in.ClientID, Authority: in.Authority,
		CredentialTypes: in.CredentialTypes, ScopeCeiling: in.ScopeCeiling, AutoRefreshToken: in.AutoRefreshToken,
		ProjectScope: in.ProjectScope, ProjectIDs: in.ProjectIDs, SecretExpiresAt: in.SecretExpiresAt,
		State: microsoftapps.StateDraft, Revision: 1, CreatedAt: time.Unix(1700000000, 0), UpdatedAt: time.Unix(1700000000, 0),
	}
	if in.Secret != nil && *in.Secret != "" {
		s.secrets[id] = *in.Secret
		app.SecretSet, app.State = true, microsoftapps.StateActive
	}
	s.apps[id] = app
	return app
}

func (s *msStore) Create(_ context.Context, in microsoftapps.Input, _ microsoftapps.Actor) (microsoftapps.App, error) {
	if s.err != nil {
		return microsoftapps.App{}, s.err
	}
	if _, err := in.Normalize(); err != nil {
		return microsoftapps.App{}, err
	}
	return s.save("11111111-1111-4111-8111-111111111111", in), nil
}

func (s *msStore) Update(_ context.Context, id string, in microsoftapps.Input, _ microsoftapps.Actor) (microsoftapps.App, error) {
	if _, ok := s.apps[id]; !ok {
		return microsoftapps.App{}, microsoftapps.ErrNotFound
	}
	if _, err := in.Normalize(); err != nil {
		return microsoftapps.App{}, err
	}
	return s.save(id, in), s.err
}

func (s *msStore) SetDisabled(_ context.Context, id string, disabled bool, _ microsoftapps.Actor) (microsoftapps.App, error) {
	app, ok := s.apps[id]
	if !ok {
		return microsoftapps.App{}, microsoftapps.ErrNotFound
	}
	app.State = microsoftapps.StateActive
	if disabled {
		app.State = microsoftapps.StateDisabled
		s.disabled = append(s.disabled, id)
	}
	s.apps[id] = app
	return app, nil
}

func (s *msStore) CustomAppsAllowed(context.Context) (bool, error) { return s.custom, nil }

func (s *msStore) SetCustomAppsAllowed(_ context.Context, allowed bool, _ microsoftapps.Actor) error {
	s.custom = allowed
	return s.err
}

type msDoer struct{ body string }

func (d msDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: http.NoBody}, errors.New("not used: " + d.body)
}

func msRouter(store *msStore) http.Handler {
	h := &Handler{}
	if store != nil {
		WithMicrosoftApps(store, msDoer{}, "https://elitea.example.com")(h)
	}
	r := chi.NewRouter()
	r.Get("/microsoft_oauth_apps", h.MicrosoftAppsList)
	r.Post("/microsoft_oauth_apps", h.MicrosoftAppsCreate)
	r.Put("/microsoft_oauth_apps/settings", h.MicrosoftAppsSettings)
	r.Put("/microsoft_oauth_apps/{id}", h.MicrosoftAppsUpdate)
	r.Post("/microsoft_oauth_apps/{id}/disable", h.MicrosoftAppsDisable)
	r.Post("/microsoft_oauth_apps/{id}/enable", h.MicrosoftAppsEnable)
	r.Post("/microsoft_oauth_apps/{id}/test", h.MicrosoftAppsTest)
	return r
}

func msDo(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "elitea.example.com"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func msBody(extra string) string {
	return `{"name":"Corp","client_id":"` + msTestClient + `","authority":"https://login.microsoftonline.com/organizations",` +
		`"credential_types":["outlook","teams"]` + extra + `}`
}

func TestMicrosoftAppsAnswer503WithoutTheStore(t *testing.T) {
	router := msRouter(nil)
	for _, call := range [][2]string{
		{http.MethodGet, "/microsoft_oauth_apps"}, {http.MethodPost, "/microsoft_oauth_apps"},
		{http.MethodPut, "/microsoft_oauth_apps/settings"}, {http.MethodPut, "/microsoft_oauth_apps/x"},
		{http.MethodPost, "/microsoft_oauth_apps/x/disable"}, {http.MethodPost, "/microsoft_oauth_apps/x/enable"},
		{http.MethodPost, "/microsoft_oauth_apps/x/test"},
	} {
		if got := msDo(t, router, call[0], call[1], `{}`); got.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", call[0], call[1], got.Code)
		}
	}
}

// The secret is write-only: it appears in no answer a route gives, whatever the
// route.
func TestTheSecretIsInNoResponse(t *testing.T) {
	store := newMSStore()
	router := msRouter(store)
	var seen []string
	record := func(r *httptest.ResponseRecorder) { seen = append(seen, r.Body.String()) }

	created := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(`,"secret":"`+msTestSecret+`"`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	record(created)
	var view microsoftAppView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil || !view.SecretSet || view.State != "active" {
		t.Fatalf("view = %+v %v", view, err)
	}
	record(msDo(t, router, http.MethodGet, "/microsoft_oauth_apps", ""))
	record(msDo(t, router, http.MethodPut, "/microsoft_oauth_apps/"+view.ID, msBody(`,"secret":"`+msTestSecret+`2"`)))
	record(msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/"+view.ID+"/disable", ""))
	record(msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/"+view.ID+"/enable", ""))
	// An invalid write echoes nothing of the body either.
	bad := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", `{"name":"x","client_id":"nope","secret":"`+msTestSecret+`"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad = %d", bad.Code)
	}
	record(bad)
	for i, body := range seen {
		if strings.Contains(body, msTestSecret) {
			t.Errorf("response %d carries the secret: %s", i, body)
		}
	}
	var fields map[string]any
	_ = json.Unmarshal([]byte(seen[0]), &fields)
	for key := range fields {
		if strings.Contains(key, "secret") && key != "secret_set" && key != "secret_expires_at" {
			t.Errorf("a view field %q could hold a secret", key)
		}
	}
}

func TestTheSecretIsTriState(t *testing.T) {
	store := newMSStore()
	router := msRouter(store)
	created := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(`,"secret":"`+msTestSecret+`"`))
	var view microsoftAppView
	_ = json.Unmarshal(created.Body.Bytes(), &view)
	path := "/microsoft_oauth_apps/" + view.ID

	msDo(t, router, http.MethodPut, path, msBody(""))
	if got := store.inputs[len(store.inputs)-1].Secret; got != nil {
		t.Errorf("an absent secret reached the store as %q; it must stay nil so the sealed one is kept", *got)
	}
	msDo(t, router, http.MethodPut, path, msBody(`,"secret":""`))
	if got := store.inputs[len(store.inputs)-1].Secret; got == nil || *got != "" {
		t.Errorf("an empty secret must reach the store as a clear, got %v", got)
	}
	msDo(t, router, http.MethodPut, path, msBody(`,"secret":"  new-secret "`))
	if got := store.inputs[len(store.inputs)-1].Secret; got == nil || *got != "new-secret" {
		t.Errorf("a secret value is trimmed and sealed, got %v", got)
	}
}

func TestValidationNamesTheFieldAndErrorsMapToStatuses(t *testing.T) {
	store := newMSStore()
	router := msRouter(store)
	for name, tc := range map[string]struct {
		body  string
		field string
	}{
		"client":    {`{"name":"x","client_id":"nope","authority":"https://login.microsoftonline.com/common","credential_types":["outlook"]}`, "client_id"},
		"authority": {`{"name":"x","client_id":"` + msTestClient + `","authority":"https://evil.example/common","credential_types":["outlook"]}`, "authority"},
		"types":     {`{"name":"x","client_id":"` + msTestClient + `","authority":"https://login.microsoftonline.com/common","credential_types":[]}`, "credential_types"},
		"date":      {msBody(`,"secret_expires_at":"soon"`), "secret_expires_at"},
	} {
		got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", tc.body)
		var body map[string]string
		_ = json.Unmarshal(got.Body.Bytes(), &body)
		if got.Code != http.StatusBadRequest || body["field"] != tc.field {
			t.Errorf("%s: %d %v", name, got.Code, body)
		}
	}
	if got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", `{"name":`); got.Code != http.StatusBadRequest {
		t.Errorf("malformed json = %d", got.Code)
	}
	if got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(`,"client_secret":"x"`)); got.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d; a misspelt secret must not be silently dropped", got.Code)
	}
	if got := msDo(t, router, http.MethodPut, "/microsoft_oauth_apps/missing", msBody("")); got.Code != http.StatusNotFound {
		t.Errorf("unknown app = %d", got.Code)
	}
	store.err = microsoftapps.ErrConflict
	if got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody("")); got.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d", got.Code)
	}
	store.err = errors.New("pq: connection refused host=db password=hunter2")
	failed := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(""))
	if failed.Code != http.StatusServiceUnavailable || strings.Contains(failed.Body.String(), "hunter2") {
		t.Errorf("a store failure = %d %s; it must not echo the driver's message", failed.Code, failed.Body)
	}
}

func TestListCarriesTheChecklistAndTheSettings(t *testing.T) {
	store := newMSStore()
	router := msRouter(store)
	empty := msDo(t, router, http.MethodGet, "/microsoft_oauth_apps", "")
	var list struct {
		Apps      []microsoftAppView                `json:"apps"`
		Total     int                               `json:"total"`
		Settings  microsoftAppSettings              `json:"settings"`
		Checklist microsoftapps.DeploymentChecklist `json:"checklist"`
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &list); err != nil || list.Total != 0 || list.Apps == nil {
		t.Fatalf("empty registry = %s %v", empty.Body, err)
	}
	if !list.Settings.CustomApps {
		t.Error("custom apps default on")
	}
	// Even with no app, an Entra administrator can read every redirect URI and
	// permission to set up.
	if list.Checklist.RedirectURI != "https://elitea.example.com/api/v2/microsoft_login:callback" || len(list.Checklist.Types) != 5 {
		t.Errorf("deployment checklist = %+v", list.Checklist)
	}

	created := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(`,"secret":"`+msTestSecret+`","scope_ceiling":{"teams":["ChannelMessage.Read.All"]}`))
	var view microsoftAppView
	_ = json.Unmarshal(created.Body.Bytes(), &view)
	if view.Checklist.RedirectURI != list.Checklist.RedirectURI || view.Checklist.AccountTypes != "multi" || !view.Checklist.NeedsAdminConsent {
		t.Errorf("app checklist = %+v", view.Checklist)
	}
	if got := view.EffectiveCeiling["outlook"]; len(got) != 1 || got[0] != "Mail.Read" {
		t.Errorf("effective ceiling = %v", view.EffectiveCeiling)
	}
	if view.ScopeCeiling["outlook"] != nil {
		t.Errorf("saved ceiling must hold only what was saved: %v", view.ScopeCeiling)
	}

	off := msDo(t, router, http.MethodPut, "/microsoft_oauth_apps/settings", `{"custom_apps":false}`)
	if off.Code != http.StatusOK || store.custom {
		t.Errorf("settings = %d custom=%v", off.Code, store.custom)
	}
	if bad := msDo(t, router, http.MethodPut, "/microsoft_oauth_apps/settings", `{}`); bad.Code != http.StatusBadRequest {
		t.Errorf("settings without a value = %d", bad.Code)
	}
}

func TestHelmManagedRowsSayWhereTheyComeFrom(t *testing.T) {
	store := newMSStore()
	store.apps["a"] = microsoftapps.App{
		ID: "a", SeedKey: "corp", Name: "Corp", ClientID: msTestClient, Authority: "https://login.microsoftonline.com/organizations",
		CredentialTypes: []string{"outlook"}, State: microsoftapps.StateDraft, ProjectScope: microsoftapps.ScopeAll,
	}
	got := msDo(t, msRouter(store), http.MethodGet, "/microsoft_oauth_apps", "")
	if !strings.Contains(got.Body.String(), `"managed_by":"helm"`) {
		t.Errorf("a seeded row must say it is managed by helm: %s", got.Body)
	}
	if strings.Contains(got.Body.String(), "seed_key") {
		t.Error("the seed key is an implementation detail")
	}
}

func TestDisableEnableAndTest(t *testing.T) {
	store := newMSStore()
	router := msRouter(store)
	created := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps", msBody(`,"secret":"`+msTestSecret+`"`))
	var view microsoftAppView
	_ = json.Unmarshal(created.Body.Bytes(), &view)

	off := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/"+view.ID+"/disable", "")
	if !strings.Contains(off.Body.String(), `"state":"disabled"`) {
		t.Errorf("disable: %s", off.Body)
	}
	on := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/"+view.ID+"/enable", "")
	if !strings.Contains(on.Body.String(), `"state":"active"`) {
		t.Errorf("enable: %s", on.Body)
	}
	if got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/nope/disable", ""); got.Code != http.StatusNotFound {
		t.Errorf("disable unknown = %d", got.Code)
	}

	result := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/"+view.ID+"/test", "")
	var test microsoftapps.TestResult
	if err := json.Unmarshal(result.Body.Bytes(), &test); err != nil || result.Code != http.StatusOK || len(test.Checks) == 0 {
		t.Fatalf("test = %d %s", result.Code, result.Body)
	}
	if test.OK {
		t.Error("the stub client cannot reach Entra, so the authority check must fail")
	}
	if got := msDo(t, router, http.MethodPost, "/microsoft_oauth_apps/nope/test", ""); got.Code != http.StatusNotFound {
		t.Errorf("test unknown = %d", got.Code)
	}
}

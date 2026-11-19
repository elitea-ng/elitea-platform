package microsoftapps

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeReader struct {
	apps      map[string]App
	secrets   map[string]Secret
	secretErr error
	// staleOnce makes the first secret read report the app moved.
	staleOnce bool
	reads     int
	attention []string
	custom    *bool
}

func (f *fakeReader) Get(_ context.Context, id string) (App, error) {
	if app, ok := f.apps[id]; ok {
		return app, nil
	}
	return App{}, ErrNotFound
}

func (f *fakeReader) List(context.Context) ([]App, error) {
	out := []App{}
	for _, app := range f.apps {
		out = append(out, app)
	}
	return out, nil
}

func (f *fakeReader) openSecret(_ context.Context, app App) (Secret, error) {
	f.reads++
	if f.staleOnce && f.reads == 1 {
		return Secret{}, errStale
	}
	if f.secretErr != nil {
		return Secret{}, f.secretErr
	}
	return f.secrets[app.ID], nil
}

func (f *fakeReader) MarkNeedsAttention(_ context.Context, id, _ string) error {
	f.attention = append(f.attention, id)
	return nil
}

func (f *fakeReader) CustomAppsAllowed(context.Context) (bool, error) {
	if f.custom == nil {
		return true, nil
	}
	return *f.custom, nil
}

func activeApp(id string) App {
	return App{
		ID: id, Name: "Corp", ClientID: testClientID, SecretSet: true, State: StateActive,
		Authority:       "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555",
		CredentialTypes: []string{TypeOutlook, TypeDataverse}, ScopeCeiling: map[string][]string{TypeOutlook: {"Mail.Read", "Mail.Send"}},
		ProjectScope: ScopeAll, AutoRefreshToken: true, Revision: 3,
	}
}

func resolverWith(apps ...App) (*Resolver, *fakeReader) {
	fake := &fakeReader{apps: map[string]App{}, secrets: map[string]Secret{}}
	for _, app := range apps {
		fake.apps[app.ID] = app
		fake.secrets[app.ID] = NewSecret(plaintextSecret)
	}
	return &Resolver{store: fake}, fake
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var refusal *ResolveError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a ResolveError", err)
	}
	return refusal.Code
}

func TestResolveHandsOutTheClientOfAnAvailableApp(t *testing.T) {
	resolver, _ := resolverWith(activeApp("a1"))
	got, err := resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeOutlook, ProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != testClientID || got.Secret.Reveal() != plaintextSecret || !got.AutoRefreshToken || got.Revision != 3 {
		t.Errorf("resolved = %+v", got)
	}
	if !reflect.DeepEqual(got.Ceiling, []string{"Mail.Read", "Mail.Send"}) {
		t.Errorf("ceiling = %v", got.Ceiling)
	}
	described, err := resolver.Describe(context.Background(), Request{AppID: "a1", CredentialType: TypeOutlook, ProjectID: 7})
	if err != nil || !described.Secret.Empty() {
		t.Errorf("Describe must not carry the secret: %+v %v", described, err)
	}
}

// The availability rule is applied at every use, and the app id and the project
// both come from the client.
func TestResolveRefusesWhatIsNotAvailableWithAStableCode(t *testing.T) {
	cases := map[string]struct {
		mutate func(*App)
		req    Request
		code   string
	}{
		"draft":      {func(a *App) { a.State = StateDraft }, Request{CredentialType: TypeOutlook, ProjectID: 1}, CodeAppDraft},
		"disabled":   {func(a *App) { a.State = StateDisabled }, Request{CredentialType: TypeOutlook, ProjectID: 1}, CodeAppDisabled},
		"attention":  {func(a *App) { a.State = StateNeedsAttention }, Request{CredentialType: TypeOutlook, ProjectID: 1}, CodeAppNeedsAttention},
		"type":       {func(a *App) {}, Request{CredentialType: TypeTeams, ProjectID: 1}, CodeTypeNotServed},
		"not a type": {func(a *App) {}, Request{CredentialType: "gmail", ProjectID: 1}, CodeTypeNotServed},
		"project": {func(a *App) { a.ProjectScope, a.ProjectIDs = ScopeSelected, []int64{2} },
			Request{CredentialType: TypeOutlook, ProjectID: 1}, CodeProjectNotServed},
	}
	for name, tc := range cases {
		app := activeApp("a1")
		tc.mutate(&app)
		resolver, fake := resolverWith(app)
		tc.req.AppID = "a1"
		_, err := resolver.Resolve(context.Background(), tc.req)
		if code := codeOf(t, err); code != tc.code {
			t.Errorf("%s: code = %s, want %s", name, code, tc.code)
		}
		if len(fake.attention) != 0 {
			t.Errorf("%s: a refusal marked the app", name)
		}
	}
	resolver, _ := resolverWith()
	_, err := resolver.Resolve(context.Background(), Request{AppID: "missing", CredentialType: TypeOutlook, ProjectID: 1})
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("a missing app: %v", err)
	}
}

func TestAnUnreadableSecretMarksTheAppAndNeverFallsBack(t *testing.T) {
	resolver, fake := resolverWith(activeApp("a1"), activeApp("a2"))
	fake.secretErr = ErrCustodyIntegrity
	_, err := resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeOutlook, ProjectID: 1})
	if code := codeOf(t, err); code != CodeSecretUnavailable {
		t.Errorf("code = %s", code)
	}
	if !reflect.DeepEqual(fake.attention, []string{"a1"}) {
		t.Errorf("marked = %v, want only the app that failed", fake.attention)
	}
	if msg := err.Error(); msg == "" || strings.Contains(msg, testClientID) || strings.Contains(msg, plaintextSecret) {
		t.Errorf("message leaks or is empty: %q", msg)
	}
}

func TestDataverseAuthorityRule(t *testing.T) {
	tenant := "11111111-2222-4333-8444-555555555555"
	want := "https://login.microsoftonline.com/" + tenant

	app := activeApp("a1")
	resolver, _ := resolverWith(app)
	got, err := resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeDataverse, ProjectID: 1, TenantID: tenant})
	if err != nil || got.Authority != want || got.Ceiling != nil {
		t.Errorf("same tenant: %+v %v", got, err)
	}
	if _, err = resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeDataverse, ProjectID: 1, TenantID: "99999999-2222-4333-8444-555555555555"}); codeOf(t, err) != CodeTenantMismatch {
		t.Errorf("another tenant: %v", err)
	}
	if _, err = resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeDataverse, ProjectID: 1}); codeOf(t, err) != CodeTenantRequired {
		t.Errorf("no tenant: %v", err)
	}

	alias := activeApp("a2")
	alias.Authority = "https://login.microsoftonline.com/organizations"
	resolver, _ = resolverWith(alias)
	got, err = resolver.Resolve(context.Background(), Request{AppID: "a2", CredentialType: TypeDataverse, ProjectID: 1, TenantID: "Contoso.Onmicrosoft.com"})
	if err != nil || got.Authority != "https://login.microsoftonline.com/contoso.onmicrosoft.com" {
		t.Errorf("alias app uses the credential's tenant: %+v %v", got, err)
	}
	// A credential cannot name an alias as its tenant: the authority would then
	// not be the credential's own.
	if _, err = resolver.Resolve(context.Background(), Request{AppID: "a2", CredentialType: TypeDataverse, ProjectID: 1, TenantID: "common"}); codeOf(t, err) != CodeTenantMismatch {
		t.Errorf("alias tenant on the credential: %v", err)
	}
	// Other types use the app's authority as it is.
	got, err = resolver.Resolve(context.Background(), Request{AppID: "a2", CredentialType: TypeOutlook, ProjectID: 1})
	if err != nil || got.Authority != alias.Authority {
		t.Errorf("outlook authority: %+v %v", got, err)
	}
}

func TestOffersListOnlyWhatACredentialFormMayUse(t *testing.T) {
	usable := activeApp("a1")
	draft := activeApp("a2")
	draft.State = StateDraft
	disabled := activeApp("a3")
	disabled.State = StateDisabled
	elsewhere := activeApp("a4")
	elsewhere.ProjectScope, elsewhere.ProjectIDs = ScopeSelected, []int64{99}
	teamsOnly := activeApp("a5")
	teamsOnly.CredentialTypes = []string{TypeTeams}

	resolver, _ := resolverWith(usable, draft, disabled, elsewhere, teamsOnly)
	offers, err := resolver.Offers(context.Background(), TypeOutlook, 7)
	if err != nil || len(offers) != 1 || offers[0].ID != "a1" {
		t.Fatalf("offers = %+v, %v", offers, err)
	}
	if !reflect.DeepEqual(offers[0].Ceiling, []string{"Mail.Read", "Mail.Send"}) {
		t.Errorf("ceiling = %v", offers[0].Ceiling)
	}
	if dataverse, _ := resolver.Offers(context.Background(), TypeDataverse, 7); len(dataverse) != 1 || dataverse[0].Ceiling != nil {
		t.Errorf("dataverse offers = %+v", dataverse)
	}

	// An empty registry offers nothing, so the form shows only Custom app.
	empty, _ := resolverWith()
	if offers, err := empty.Offers(context.Background(), TypeOutlook, 7); err != nil || len(offers) != 0 || offers == nil {
		t.Errorf("empty registry = %#v, %v", offers, err)
	}
	if _, err := empty.Offers(context.Background(), "gmail", 7); err == nil {
		t.Error("an unknown type was answered")
	}
}

func TestCustomAppsAllowedDefaultsOn(t *testing.T) {
	resolver, fake := resolverWith()
	if !resolver.CustomAppsAllowed(context.Background()) {
		t.Error("custom apps are on by default")
	}
	off := false
	fake.custom = &off
	if resolver.CustomAppsAllowed(context.Background()) {
		t.Error("the admin switch was ignored")
	}
}

func TestAnAppThatMovedUnderTheSecretReadIsDescribedAgainAndNotMarked(t *testing.T) {
	resolver, fake := resolverWith(activeApp("a1"))
	fake.staleOnce = true
	got, err := resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeOutlook, ProjectID: 1})
	if err != nil || got.Secret.Reveal() != plaintextSecret || fake.reads != 2 {
		t.Fatalf("resolved = %+v %v after %d reads", got, err, fake.reads)
	}
	if len(fake.attention) != 0 {
		t.Errorf("a stale read marked the app: %v", fake.attention)
	}
	// An app that keeps moving is reported as stale, never as corrupt.
	fake.secretErr = errStale
	fake.reads = 0
	if _, err := resolver.Resolve(context.Background(), Request{AppID: "a1", CredentialType: TypeOutlook, ProjectID: 1}); !errors.Is(err, errStale) || fake.reads != 2 {
		t.Errorf("a loop: %v after %d reads", err, fake.reads)
	}
	if len(fake.attention) != 0 {
		t.Errorf("a stale read marked the app: %v", fake.attention)
	}
}

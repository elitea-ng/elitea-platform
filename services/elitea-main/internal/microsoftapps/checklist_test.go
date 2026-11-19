package microsoftapps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var checklistNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func TestRedirectURIIsTheOneCallbackOfTheDeployment(t *testing.T) {
	if got := RedirectURI("https://elitea.example.com/"); got != "https://elitea.example.com/api/v2/microsoft_login:callback" {
		t.Errorf("redirect = %q", got)
	}
	for origin, issue := range map[string]string{
		"https://elitea.example.com":  "",
		"http://localhost:8080":       "",
		"http://127.0.0.1:3000":       "",
		"http://elitea.example.com":   "https_required",
		"http://192.168.1.20:8080":    "https_required",
		"https://elitea.example.com:": "",
	} {
		if got := RedirectURIIssue(RedirectURI(origin)); got != issue {
			t.Errorf("RedirectURIIssue(%q) = %q, want %q", origin, got, issue)
		}
	}
}

func TestSecretExpiryStates(t *testing.T) {
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		at   *time.Time
		want ExpiryState
	}{
		"none":     {nil, ExpiryNone},
		"expired":  {ptr(checklistNow.Add(-day)), ExpiryExpired},
		"expiring": {ptr(checklistNow.Add(29 * day)), ExpiryExpiring},
		"edge":     {ptr(checklistNow.Add(30 * day)), ExpiryExpiring},
		"ok":       {ptr(checklistNow.Add(60 * day)), ExpiryOK},
	} {
		if got, _ := SecretExpiry(tc.at, checklistNow); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestBuildChecklistListsEveryRedirectAndPermission(t *testing.T) {
	app := activeApp("a1")
	app.CredentialTypes = []string{TypeTeams, TypeDataverse}
	app.ScopeCeiling = map[string][]string{TypeTeams: {"ChannelMessage.Read.All"}}
	app.SecretExpiresAt = ptr(checklistNow.Add(10 * 24 * time.Hour))
	list := BuildChecklist(app, "https://elitea.example.com", checklistNow)

	if list.RedirectURI != "https://elitea.example.com/api/v2/microsoft_login:callback" || list.RedirectIssue != "" {
		t.Errorf("redirect = %q %q", list.RedirectURI, list.RedirectIssue)
	}
	if list.AccountTypes != "single" || !list.NeedsAdminConsent {
		t.Errorf("account types %q consent %v", list.AccountTypes, list.NeedsAdminConsent)
	}
	want := "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555/adminconsent?client_id=" + testClientID +
		"&redirect_uri=https%3A%2F%2Felitea.example.com%2Fapi%2Fv2%2Fmicrosoft_login%3Acallback"
	if list.AdminConsentURL != want {
		t.Errorf("consent url = %q", list.AdminConsentURL)
	}
	if list.SecretExpiry != "expiring" || list.SecretDaysLeft != 10 {
		t.Errorf("expiry = %s %d", list.SecretExpiry, list.SecretDaysLeft)
	}

	multi := app
	multi.Authority = "https://login.microsoftonline.com/organizations"
	list = BuildChecklist(multi, "https://elitea.example.com", checklistNow)
	if list.AccountTypes != "multi" || !strings.Contains(list.AdminConsentURL, "/{tenant-id}/adminconsent") {
		t.Errorf("multi-tenant checklist = %q %q", list.AccountTypes, list.AdminConsentURL)
	}
	if http := BuildChecklist(app, "http://elitea.example.com", checklistNow); http.RedirectIssue != "https_required" {
		t.Errorf("plain http origin issue = %q", http.RedirectIssue)
	}
}

func TestDeploymentChecklistNeedsNoApp(t *testing.T) {
	list := BuildDeploymentChecklist("https://elitea.example.com")
	if list.RedirectURI == "" || len(list.Types) != len(CredentialTypes()) {
		t.Fatalf("checklist = %+v", list)
	}
	byType := map[string]TypePermissions{}
	for _, entry := range list.Types {
		byType[entry.Type] = entry
	}
	if len(byType[TypeOutlook].Offered) != 6 || len(byType[TypeTeams].Offered) != 8 || len(byType[TypeMSGraph].Offered) != 0 {
		t.Errorf("offered lists = %+v", byType)
	}
	if byType[TypeDataverse].API != "Dynamics CRM" || byType[TypeDataverse].Default[0] != "user_impersonation" {
		t.Errorf("dataverse = %+v", byType[TypeDataverse])
	}
	consent := false
	for _, p := range byType[TypeTeams].Offered {
		consent = consent || (p.Name == "ChannelMessage.Read.All" && p.AdminConsent)
	}
	if !consent {
		t.Error("ChannelMessage.Read.All must be marked as needing admin consent")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func respond(status int, body string) HTTPDoer {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
}

const goodMetadata = `{"authorization_endpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/authorize","token_endpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token"}`

func checkByID(result TestResult, id string) Check {
	for _, check := range result.Checks {
		if check.ID == id {
			return check
		}
	}
	return Check{ID: id, Detail: "missing"}
}

func TestTestReportsEachCheck(t *testing.T) {
	app := activeApp("a1")
	result := Test(context.Background(), app, respond(200, goodMetadata), "https://elitea.example.com", "https://elitea.example.com", checklistNow)
	if !result.OK {
		t.Fatalf("a healthy app failed: %+v", result)
	}
	for _, id := range []string{"client_id", "authority", "redirect_uri", "secret"} {
		if !checkByID(result, id).OK {
			t.Errorf("%s did not pass: %+v", id, result)
		}
	}

	cases := map[string]struct {
		doer       HTTPDoer
		mutate     func(*App)
		request    string
		deployment string
		check      string
		detail     string
	}{
		"unknown tenant": {respond(400, "{}"), nil, "https://e.example", "https://e.example", "authority", "http_400"},
		"unreachable": {roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") }),
			nil, "https://e.example", "https://e.example", "authority", "unreachable"},
		"not entra metadata": {respond(200, `{"authorization_endpoint":"https://evil.example/a","token_endpoint":"https://evil.example/t"}`),
			nil, "https://e.example", "https://e.example", "authority", "not_entra_metadata"},
		"plain http": {respond(200, goodMetadata), nil, "http://e.example", "http://e.example", "redirect_uri", "https_required"},
		"wrong host": {respond(200, goodMetadata), nil, "https://admin-alias.example", "https://e.example", "redirect_uri", "host_mismatch"},
		"no secret":  {respond(200, goodMetadata), func(a *App) { a.SecretSet = false }, "https://e.example", "https://e.example", "secret", "not_set"},
		"expired":    {respond(200, goodMetadata), func(a *App) { a.SecretExpiresAt = ptr(checklistNow.Add(-time.Hour)) }, "https://e.example", "https://e.example", "secret_expiry", "expired"},
	}
	for name, tc := range cases {
		app := activeApp("a1")
		if tc.mutate != nil {
			tc.mutate(&app)
		}
		result := Test(context.Background(), app, tc.doer, tc.request, tc.deployment, checklistNow)
		check := checkByID(result, tc.check)
		if result.OK || check.OK || check.Detail != tc.detail {
			t.Errorf("%s: %+v", name, result)
		}
	}

	expiring := activeApp("a1")
	expiring.SecretExpiresAt = ptr(checklistNow.Add(5 * 24 * time.Hour))
	result = Test(context.Background(), expiring, respond(200, goodMetadata), "https://e.example", "https://e.example", checklistNow)
	if check := checkByID(result, "secret_expiry"); !result.OK || !check.OK || !check.Warn {
		t.Errorf("an expiring secret passes with a warning: %+v", result)
	}
}

func TestTestReadsMetadataFromTheAuthorityOnly(t *testing.T) {
	var requested string
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = req.URL.String()
		rec := httptest.NewRecorder()
		_, _ = rec.WriteString(goodMetadata)
		return rec.Result(), nil
	})
	Test(context.Background(), activeApp("a1"), doer, "https://e.example", "https://e.example", checklistNow)
	if requested != "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555/v2.0/.well-known/openid-configuration" {
		t.Errorf("requested %q", requested)
	}
}

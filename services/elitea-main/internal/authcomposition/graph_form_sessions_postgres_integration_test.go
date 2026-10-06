package authcomposition

// A Form session minted BEFORE an operator switched Form sign-in off must stop
// authorizing at once (docs/UPGRADING.md), not live on for the rest of
// cookie.lifetime_seconds. TestFormSessionsRefusedWhileFormSignInIsDisabled
// covers the decorator; these tests cover the WIRING: a real session in the
// graph's own session store, a real active user row, sent through both
// composed kernels (Main via AuthorizeMain, Direct via Routes()). Dropping the
// decorator in newFormGraph, or handing the bare flow to either kernel, turns
// the disabled case green here into an authorized session.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authstatetest"
)

func TestAnExistingFormSessionStopsAuthorizingWhenFormSignInIsDisabled(t *testing.T) {
	pool := newFormSessionGraphPool(t)
	var userID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO auth_core__user (email, name) VALUES ('form-user@example.test', 'Form User') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		enabled    bool
		provider   string
		authorized bool
	}{
		{name: "Form session, Form sign-in enabled", enabled: true, provider: browserapp.FormProviderName, authorized: true},
		{name: "Form session, Form sign-in disabled", enabled: false, provider: browserapp.FormProviderName, authorized: false},
		{name: "OIDC session, Form sign-in disabled", enabled: false, provider: "oidc", authorized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph, config, sessionID := newGraphWithSession(t, pool, test.enabled, test.provider, userID)

			request := publicMainRequest("/api/v2/private")
			request.BrowserSession = forwardapp.BrowserSessionInput{
				Present: true, ID: sessionID, Reference: sessionID,
			}
			decision, err := graph.AuthorizeMain(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			mainAuthorized := decision.Kind == forwardapp.DecisionAllow &&
				decision.Authentication.Type == forwardapp.AuthenticationUser
			if mainAuthorized != test.authorized {
				t.Fatalf("Main kernel: authorized=%v, want %v (decision %+v)", mainAuthorized, test.authorized, decision)
			}

			direct := httptest.NewRequest(http.MethodGet, "http://auth-internal/check", nil)
			direct.RemoteAddr = "10.1.2.3:1234"
			direct.Header.Set("X-Forwarded-For", "203.0.113.7")
			direct.Header.Set("X-Forwarded-Method", http.MethodGet)
			direct.Header.Set("X-Forwarded-Proto", "https")
			direct.Header.Set("X-Forwarded-Host", "elitea.example")
			direct.Header.Set("X-Forwarded-Uri", "/api/v2/private")
			direct.AddCookie(&http.Cookie{Name: config.Cookie.Name, Value: browserapi.CookieValuePrefix + sessionID})
			response := httptest.NewRecorder()
			graph.Routes().ServeHTTP(response, direct)
			directAuthorized := response.Code == http.StatusOK
			if directAuthorized != test.authorized {
				t.Fatalf("Direct kernel: status=%d, want authorized=%v (location %q)",
					response.Code, test.authorized, response.Header().Get("Location"))
			}
		})
	}
}

func newGraphWithSession(
	t *testing.T,
	pool *pgxpool.Pool,
	formSignInEnabled bool,
	provider string,
	userID int64,
) (*FormGraph, Config, string) {
	t.Helper()
	config := writeMaterialFixture(t)
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL:           pool,
			FormSignInEnabled:    formSignInEnabled,
			MainRoutePublicRules: []forwardapp.PublicRule{},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	// The same store and lifetimes newFormGraph composes, on the same table,
	// so the session is exactly one an earlier sign-in left behind.
	store, err := authsession.NewPostgresStore(pool, authsession.Config{
		TTL:         time.Duration(config.Cookie.LifetimeSeconds) * time.Second,
		PreLoginTTL: preLoginSessionLifetime,
	})
	if err != nil {
		t.Fatal(err)
	}
	expiration := time.Now().UTC().Add(time.Hour)
	sessionID, err := store.Create(context.Background(), sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Expiration:         &expiration,
		Provider:           &provider,
		ProviderAttributes: json.RawMessage(`{}`),
		UserID:             &userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph, config, sessionID
}

// newFormSessionGraphPool creates a private empty database and migrates it the
// way cmd/elitea-migrate does on a first install.
func newFormSessionGraphPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return authstatetest.Pool(t, 8)
}

// With Form sign-in off, the Form handler the graph composes must refuse the
// configured login and password. The graph still mounts the Form routes inside
// Routes() (the edge composition needs the router), so this drives the real
// sign-in exchange through them: begin, then POST the configured credentials.
// Enabled, the user is provisioned; disabled, nothing is, and the browser is
// sent back to the login page. A field holding a COPY of the handler's
// provider could not tell these apart if the handler were handed the
// configured list again.
func TestTheComposedFormHandlerRefusesConfiguredPasswordsWhenDisabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := newFormSessionGraphPool(t)
			config := writeMaterialFixture(t)
			graph, err := newFormGraph(
				context.Background(),
				config,
				FormGraphDependencies{
					PostgreSQL:           pool,
					FormSignInEnabled:    test.enabled,
					MainRoutePublicRules: []forwardapp.PublicRule{},
				},
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			begin := formExchangeRequest(http.MethodGet, browserapi.LoginPath, nil)
			beginResponse := httptest.NewRecorder()
			graph.Routes().ServeHTTP(beginResponse, begin)
			location, err := url.Parse(beginResponse.Header().Get("Location"))
			if beginResponse.Code != http.StatusFound || err != nil {
				t.Fatalf("begin = %d location=%q", beginResponse.Code, beginResponse.Header().Get("Location"))
			}
			target := location.Query().Get("target_to")
			var sessionCookie *http.Cookie
			for _, cookie := range beginResponse.Result().Cookies() {
				if cookie.Name == config.Cookie.Name && cookie.Value != "" {
					sessionCookie = cookie
				}
			}
			if target == "" || sessionCookie == nil {
				t.Fatalf("begin set no transaction or session: location=%q cookies=%v",
					location, beginResponse.Result().Cookies())
			}
			// Begin wrote its state to PostgreSQL: one pre-login session that
			// lives five minutes, not the cookie's day, and one transaction.
			var preLoginSeconds float64
			if err := pool.QueryRow(context.Background(), `
				SELECT extract(epoch FROM max(expires_at) - now()) FROM elitea_auth.form_sessions`,
			).Scan(&preLoginSeconds); err != nil || preLoginSeconds <= 240 || preLoginSeconds > 300 {
				t.Fatalf("pre-login session lifetime = %.0fs, %v; want five minutes", preLoginSeconds, err)
			}
			if count := formStateRows(t, pool, "form_login_transactions"); count != 1 {
				t.Fatalf("login transactions after begin = %d, want 1", count)
			}

			form := url.Values{
				"target":   {target},
				"login":    {"admin"},
				"password": {"correct horse battery staple"},
			}
			submit := formExchangeRequest(http.MethodPost, browserapi.FormAuthorizePath,
				strings.NewReader(form.Encode()))
			submit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			submit.AddCookie(&http.Cookie{Name: sessionCookie.Name, Value: sessionCookie.Value})
			submitResponse := httptest.NewRecorder()
			graph.Routes().ServeHTTP(submitResponse, submit)

			var provisioned int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM auth_core__user WHERE email = 'admin@example.test'`,
			).Scan(&provisioned); err != nil {
				t.Fatal(err)
			}
			signedIn := provisioned == 1
			if signedIn != test.enabled {
				t.Fatalf("Form sign-in enabled=%v, yet the configured password signed in=%v (status %d, location %q)",
					test.enabled, signedIn, submitResponse.Code, submitResponse.Header().Get("Location"))
			}
			if !test.enabled && submitResponse.Code != http.StatusFound {
				t.Fatalf("a refused Form sign-in must send the browser back to login, got %d", submitResponse.Code)
			}
			// Both stages were admitted through the PostgreSQL limiter: the
			// global window, form_begin's client window, and form_credential's
			// client and login windows.
			if count := formStateRows(t, pool, "browser_attempt_windows"); count != 4 {
				t.Fatalf("attempt windows = %d, want 4", count)
			}
			if test.enabled {
				// The one-time transaction is consumed, and the authenticated
				// session (rotated to a fresh ID) lives the cookie lifetime.
				if count := formStateRows(t, pool, "form_login_transactions"); count != 0 {
					t.Fatalf("login transactions after sign-in = %d, want 0", count)
				}
				var authenticatedSeconds float64
				if err := pool.QueryRow(context.Background(), `
					SELECT extract(epoch FROM max(expires_at) - now()) FROM elitea_auth.form_sessions`,
				).Scan(&authenticatedSeconds); err != nil || authenticatedSeconds <= float64(config.Cookie.LifetimeSeconds-60) {
					t.Fatalf("authenticated session lifetime = %.0fs, %v; want the cookie lifetime", authenticatedSeconds, err)
				}
			}
		})
	}
}

func formExchangeRequest(method, path string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, "http://auth-internal"+path, body)
	request.RemoteAddr = "10.1.2.3:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example")
	return request
}

func formStateRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM elitea_auth."+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

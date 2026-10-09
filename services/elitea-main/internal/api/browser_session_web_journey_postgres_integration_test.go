package api

// A browser session against the REAL session table, across the requests the
// web app makes after sign-in.
//
// WHY THIS EXISTS. Post-merge verification of main 1ab920dde reported browser
// sessions refused with `session_unknown` 30-40 s after sign-in while the row
// was live. The cause was not this process: two standalone stacks were
// browsed on `localhost:<port>` from one browser profile, cookies ignore the
// port, and each sign-in replaced the other stack's `elitea_session`
// (deploy/scripts/standalone-stack.sh STANDALONE_HOST). This test pins both
// halves of that diagnosis so a real regression cannot hide behind it again:
//
//  1. a session minted the way the OIDC callback mints it stays admitted on
//     every request of the web app's post-sign-in burst, past the 30 s
//     EdgeAuth projection lifetime, past the last_seen_at touch interval and
//     after five idle minutes, and its row moves forward and is never revoked;
//  2. a well-formed cookie this deployment never issued — what a second stack
//     on the same host leaves in the jar — is refused, and refusing it does
//     not end the session it displaced.
//
// It runs the production PrincipalValidator and PostgresStore against the
// migrated shared schema. ci-go.yml supplies ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// webPostSignInRequests is the burst elitea-web sends once /app/ loads after
// the OIDC callback, captured from a real browser on a standalone stack
// (source mapping browser-session-cookie-host-20261009.md). Only the paths
// this test's RouterConfig registers are listed: an unregistered path answers
// 404 before any authentication runs and would assert nothing.
var webPostSignInRequests = []string{
	"/api/v2/projects/project/default/99",
	"/api/v2/support_assistant/config/",
	"/api/v2/elitea_core/platform_settings/prompt_lib",
	"/api/v2/social/author",
	"/api/v2/elitea_core/runtime_capabilities",
	"/api/v2/auth/permissions/prompt_lib/1",
	"/api/v2/notifications/notifications/prompt_lib/1?only_new=true&limit=1&offset=0",
	"/api/v2/configurations/models/1?include_shared=true",
	"/api/v2/elitea_core/toolkits/prompt_lib/1",
	"/api/v2/auth/token/",
}

// sessionJourneyServedPath is the route Main named in the refused requests.
const sessionJourneyServedPath = "/api/v2/social/author"

type journeyClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *journeyClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *journeyClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestABrowserSessionStaysValidAcrossTheWebAppsRequestsAfterSignIn(t *testing.T) {
	pool := newBrowserSessionJourneyPool(t)
	ctx := context.Background()

	store, err := browsersession.NewPostgresStore(pool)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	dial := &journeyClock{now: time.Now().UTC().Truncate(time.Second)}
	// The policy the standalone stack logs at start-up: 8 h idle, 7 d absolute.
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: 8 * time.Hour, AbsoluteLifetime: 7 * 24 * time.Hour,
	}, browsersession.WithClock(dial.Now))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router := NewRouter(RouterConfig{
		SessionSecret:      "session-secret",
		PrincipalValidator: authsvc.NewPrincipalValidator(pool),
		Auth:               AuthDeps{SessionStore: manager},
	})

	value, err := manager.Create(ctx, browsersession.NewSession{
		UserID: 3, Email: "admin@centry.user", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, err := browsersession.ParseCookieValue(value)
	if err != nil {
		t.Fatalf("ParseCookieValue: %v", err)
	}

	burst := func(when string) {
		t.Helper()
		for _, path := range webPostSignInRequests {
			status := serveWithSessionCookie(t, router, path, value)
			if status == http.StatusUnauthorized {
				t.Fatalf("%s: %s refused a live browser session (401)", when, path)
			}
			if status == http.StatusNotFound {
				t.Fatalf("%s: %s is not routed by this composition, so it proves nothing; drop it from the list", when, path)
			}
			// Not 401 alone: the route the field logs named must reach its
			// handler as the session's user, so the burst cannot pass on
			// routes this composition does not register.
			if path == sessionJourneyServedPath && status != http.StatusOK {
				t.Fatalf("%s: %s answered %d, want 200", when, path, status)
			}
		}
	}

	burst("right after sign-in")
	// Past IdentityProjectionLifetime (30 s) and the reported 30-40 s window.
	dial.advance(40 * time.Second)
	burst("40 s after sign-in")
	// Past browsersession.TouchInterval: this burst must move last_seen_at.
	dial.advance(25 * time.Second)
	burst("65 s after sign-in")
	touchedAt := dial.Now()
	// Idle, then navigate and reload: the same burst again.
	dial.advance(5 * time.Minute)
	burst("after five idle minutes")
	burst("after a reload")

	var lastSeen time.Time
	var revoked bool
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at, revoked_at IS NOT NULL
		FROM elitea_auth.browser_sessions WHERE id = $1`, id).Scan(&lastSeen, &revoked); err != nil {
		t.Fatalf("read the session row: %v", err)
	}
	if revoked {
		t.Fatal("the session row was revoked by ordinary requests")
	}
	if lastSeen.Before(touchedAt) {
		t.Fatalf("last_seen_at = %s, want >= %s (the first burst past the touch interval); "+
			"a session in use whose idle clock never moves expires while in use", lastSeen, touchedAt)
	}

	// A second deployment on the same browser host mints its own cookie under
	// the same name. This deployment has no row for it.
	otherStack, err := browsersession.NewManager(&routeTestSessions{rows: map[string]browsersession.Session{}},
		browsersession.Policy{IdleTimeout: time.Hour, AbsoluteLifetime: time.Hour})
	if err != nil {
		t.Fatalf("NewManager (other stack): %v", err)
	}
	foreign, err := otherStack.Create(ctx, browsersession.NewSession{
		UserID: 3, Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create (other stack): %v", err)
	}
	if status := serveWithSessionCookie(t, router, sessionJourneyServedPath, foreign); status != http.StatusUnauthorized {
		t.Fatalf("a cookie this deployment never issued answered %d, want 401", status)
	}
	burst("after a foreign cookie was refused")
}

// newBrowserSessionJourneyPool is an isolated database with the bootstrap
// dump, the ledgered shared corpus (it creates elitea_auth.browser_sessions)
// and the account the session names.
func newBrowserSessionJourneyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("ELITEA_TEST_DATABASE_URL") == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the browser session journey test")
	}
	pool := newStatusOKIntegrationPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	initial, err := os.ReadFile("../infra/db/migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES (3, 'admin@centry.user', 'Admin')
ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed the session's account: %v", err)
	}
	return pool
}

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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
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
	server := miniredis.RunT(t)
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL:           pool,
			FormSignInEnabled:    formSignInEnabled,
			MainRoutePublicRules: []forwardapp.PublicRule{},
		},
		func(context.Context, Config, *materializedFiles) (*redis.Client, error) {
			return redis.NewClient(&redis.Options{Addr: server.Addr()}), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graph.Close() })

	// The same store, prefix and TTL newFormGraph composes, so the session is
	// exactly one a pre-upgrade sign-in left behind.
	store, err := authsession.NewRedisStore(
		redis.NewClient(&redis.Options{Addr: server.Addr()}),
		authsession.Config{
			KeyPrefix: config.Redis.KeyPrefix + "session:",
			TTL:       time.Duration(config.Cookie.LifetimeSeconds) * time.Second,
		},
	)
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
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf("elitea_form_sessions_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

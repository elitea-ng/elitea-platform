package provideroauth

// Shared 0190 (issue #1121): provider OAuth custody, the consent flow and
// single-flight refresh across replicas, over the real tables and the
// security audit outbox. Three TokenSources on three separate pools stand
// for three Main replicas. CI-only: skipped without the integration
// database.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/refresh"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	oauthURLEnv    = "ELITEA_TEST_DATABASE_URL"
	oauthBootstrap = "../infra/db/migrations/001_initial.sql"

	tTenant  = int64(7)
	tUser    = int64(11)
	tBinding = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"
	tTarget  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22"
)

var (
	oauthTemplateOnce sync.Once
	oauthTemplate     string
	oauthTemplateErr  error
)

// newOAuthDatabase creates a database from the migrated template and
// answers its connection config.
func newOAuthDatabase(t *testing.T) *pgxpool.Config {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(oauthURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the provider OAuth integration test", oauthURLEnv)
	}
	oauthTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(oauthBootstrap)
		if err != nil {
			oauthTemplateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			oauthTemplateErr = err
			return
		}
		defer admin.Close()
		oauthTemplate, oauthTemplateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	require.NoError(t, oauthTemplateErr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_provideroauth_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, oauthTemplate, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return config
}

func openPool(t *testing.T, config *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), config.Copy())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func masterKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func newTestGrants(t *testing.T, pool *pgxpool.Pool) *Grants {
	t.Helper()
	audit, err := securityaudit.NewPostgresAppender("elitea-main-test")
	require.NoError(t, err)
	g, err := NewGrants(pool, masterKey(), audit)
	require.NoError(t, err)
	return g
}

// rotatingProvider is an Atlassian-like token endpoint: every refresh
// token is single use and is replaced on each refresh. Using an old one is
// invalid_grant, which is what two replicas refreshing at once would hit.
type rotatingProvider struct {
	mu            sync.Mutex
	refreshToken  string
	generation    int
	refreshCalls  atomic.Int32
	invalidGrants atomic.Int32
	delay         time.Duration
	// Consent half: the challenge each authorization URL carried.
	challenges  map[string]string
	redirectURI string
	exchanges   atomic.Int32
}

func newRotatingProvider(t *testing.T, redirectURI string) (*rotatingProvider, *httptest.Server) {
	t.Helper()
	p := &rotatingProvider{refreshToken: "rt-0", challenges: map[string]string{}, redirectURI: redirectURI, delay: 40 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	return p, srv
}

func (p *rotatingProvider) serve(w http.ResponseWriter, r *http.Request) {
	var fields map[string]string
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch fields["grant_type"] {
	case "refresh_token":
		p.refreshCalls.Add(1)
		time.Sleep(p.delay)
		p.mu.Lock()
		defer p.mu.Unlock()
		if fields["refresh_token"] != p.refreshToken {
			p.invalidGrants.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		p.generation++
		p.refreshToken = fmt.Sprintf("rt-%d", p.generation)
		_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":%q,"expires_in":3600,"token_type":"Bearer"}`,
			p.generation, p.refreshToken)
	case "authorization_code":
		p.exchanges.Add(1)
		p.mu.Lock()
		challenge, ok := p.challenges[fields["code"]]
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(fields["code_verifier"]))
		if !ok || fields["code_verifier"] == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge ||
			fields["redirect_uri"] != p.redirectURI {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at-consent","refresh_token":"rt-0","expires_in":3600,"scope":"read:jira-work offline_access","token_type":"Bearer"}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unsupported_grant_type"}`)
	}
}

// authorize records the challenge of an authorization URL under a code,
// as the provider would when the user approves.
func (p *rotatingProvider) authorize(t *testing.T, authorizationURL, code string) string {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, p.redirectURI, q.Get("redirect_uri"))
	p.mu.Lock()
	p.challenges[code] = q.Get("code_challenge")
	p.mu.Unlock()
	return q.Get("state")
}

func jiraClient(t *testing.T, tokenURL, redirectURI string) *Client {
	t.Helper()
	c, err := NewClient(ProviderConfig{
		Provider: "jira", Flavor: FlavorAtlassian, ClientID: "client", ClientSecret: "client-secret",
		AuthorizeURL: tokenURL + "/authorize", TokenURL: tokenURL + "/oauth/token", RedirectURI: redirectURI,
		Scopes: []string{"read:jira-work", "offline_access"}, Timeout: 5 * time.Second,
	}, nil)
	require.NoError(t, err)
	return c
}

const tRedirect = "http://127.0.0.1:9/api/v2/credential_consent/jira:callback"

var tKey = GrantKey{TenantProjectID: tTenant, Provider: "jira", TargetBindingID: tTarget, SubjectUserID: tUser, AgentBindingID: tBinding}

// replica is one Main process: its own pool, grants, leases, coordinator
// and token source.
func replica(t *testing.T, config *pgxpool.Config, client *Client, name string) *TokenSource {
	t.Helper()
	pool := openPool(t, config)
	leases, err := refresh.NewLeases(pool)
	require.NoError(t, err)
	coordinator, err := refresh.NewCoordinator(leases, refresh.Config{
		Holder: name, LeaseTTL: 10 * time.Second, MaxWait: 20 * time.Second,
		InitialBackoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	source, err := NewTokenSource(TokenSourceConfig{
		Grants: newTestGrants(t, pool), Leases: leases, Coordinator: coordinator, Clients: []*Client{client},
	})
	require.NoError(t, err)
	return source
}

// seedGrant stores a consented grant whose access token is inside the
// refresh skew.
func seedGrant(t *testing.T, g *Grants) string {
	t.Helper()
	id, err := g.save(context.Background(), consented{
		Key: tKey, RedirectURI: tRedirect,
		Tokens: TokenResponse{AccessToken: "at-0", RefreshToken: "rt-0", ExpiresAt: time.Now().Add(30 * time.Second)},
	})
	require.NoError(t, err)
	return id
}

// A re-consent whose token response carries no refresh token keeps the
// grant's refresh token; one that carries a new one replaces it.
func TestReconsentWithoutARefreshTokenKeepsTheStoredOne(t *testing.T) {
	pool := openPool(t, newOAuthDatabase(t))
	g := newTestGrants(t, pool)
	id := seedGrant(t, g)
	again, err := g.save(context.Background(), consented{
		Key: tKey, RedirectURI: tRedirect, Tokens: TokenResponse{AccessToken: "at-1"},
	})
	require.NoError(t, err)
	require.Equal(t, id, again)
	snap, err := g.load(context.Background(), tKey)
	require.NoError(t, err)
	stored, err := g.openRefresh(snap)
	require.NoError(t, err)
	require.Equal(t, "rt-0", stored)

	_, err = g.save(context.Background(), consented{
		Key: tKey, RedirectURI: tRedirect, Tokens: TokenResponse{AccessToken: "at-2", RefreshToken: "rt-2"},
	})
	require.NoError(t, err)
	snap, err = g.load(context.Background(), tKey)
	require.NoError(t, err)
	stored, err = g.openRefresh(snap)
	require.NoError(t, err)
	require.Equal(t, "rt-2", stored)
}

func expireAccessToken(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE elitea_identity.provider_oauth_grants SET access_token_expires_at = clock_timestamp() + interval '10 seconds'`)
	require.NoError(t, err)
}

// Acceptance (#1121): 20 concurrent refreshes across 3 Main replicas make
// exactly one provider refresh call, and every caller gets the new token.
// The provider rotates refresh tokens (Atlassian 3LO), so a second refresh
// with the old token would be invalid_grant; none happens, round after
// round.
func TestConcurrentRefreshAcrossReplicasIsSingleFlight(t *testing.T) {
	config := newOAuthDatabase(t)
	provider, srv := newRotatingProvider(t, tRedirect)
	client := jiraClient(t, srv.URL, tRedirect)
	replicas := []*TokenSource{
		replica(t, config, client, "main-0"), replica(t, config, client, "main-1"), replica(t, config, client, "main-2"),
	}
	admin := openPool(t, config)
	seedGrant(t, newTestGrants(t, admin))

	const rounds, callers = 4, 20
	for round := 1; round <= rounds; round++ {
		if round > 1 {
			expireAccessToken(t, admin)
		}
		start := make(chan struct{})
		tokens := make([]string, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				token, err := replicas[i%len(replicas)].AccessToken(context.Background(), tKey)
				tokens[i], errs[i] = token.Token, err
			}()
		}
		close(start)
		wg.Wait()
		want := fmt.Sprintf("at-%d", round)
		for i := range callers {
			require.NoError(t, errs[i], "round %d caller %d", round, i)
			require.Equal(t, want, tokens[i], "round %d caller %d", round, i)
		}
		require.EqualValues(t, round, provider.refreshCalls.Load(), "round %d: provider refresh calls", round)
	}
	require.Zero(t, provider.invalidGrants.Load(), "a rotated refresh token was used twice")

	// The rotated refresh token was written with the access token.
	var generation int64
	require.NoError(t, admin.QueryRow(context.Background(),
		`SELECT generation FROM elitea_identity.provider_oauth_grants`).Scan(&generation))
	require.EqualValues(t, 1+rounds, generation)
	snap, err := newTestGrants(t, admin).load(context.Background(), tKey)
	require.NoError(t, err)
	stored, err := newTestGrants(t, admin).openRefresh(snap)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("rt-%d", rounds), stored)
}

// Acceptance: a write-back with a stale fence is refused by the CAS and
// writes nothing; the current holder's write-back lands.
func TestStaleFenceWriteBackIsRejected(t *testing.T) {
	config := newOAuthDatabase(t)
	pool := openPool(t, config)
	leases, err := refresh.NewLeases(pool)
	require.NoError(t, err)
	ctx := context.Background()
	partition := refresh.Partition("test", "stale-fence")

	first, ok, err := leases.Acquire(ctx, partition, "main-0", 50*time.Millisecond, 1)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = leases.Acquire(ctx, partition, "main-1", time.Minute, 1)
	require.NoError(t, err)
	require.False(t, ok, "a live lease was taken")

	time.Sleep(100 * time.Millisecond)
	second, ok, err := leases.Acquire(ctx, partition, "main-1", time.Minute, 1)
	require.NoError(t, err)
	require.True(t, ok, "an expired lease was not taken over")
	require.Greater(t, second.Fence, first.Fence)

	wrote := false
	err = leases.Commit(ctx, first, func(context.Context, pgx.Tx) error { wrote = true; return nil })
	require.ErrorIs(t, err, refresh.ErrStaleFence)
	require.False(t, wrote, "the stale holder's write ran")

	require.NoError(t, leases.Commit(ctx, second, func(context.Context, pgx.Tx) error { wrote = true; return nil }))
	require.True(t, wrote)
	// The commit released the lease: the next acquirer gets it at once.
	_, ok, err = leases.Acquire(ctx, partition, "main-2", time.Minute, 2)
	require.NoError(t, err)
	require.True(t, ok)
}

// A stale holder's grant write is refused too: the token source writes the
// grant only inside the fenced transaction.
func TestStaleFenceGrantWriteLeavesTheGrant(t *testing.T) {
	config := newOAuthDatabase(t)
	pool := openPool(t, config)
	grants := newTestGrants(t, pool)
	seedGrant(t, grants)
	leases, err := refresh.NewLeases(pool)
	require.NoError(t, err)
	ctx := context.Background()
	partition := refresh.Partition("test", "grant")
	snap, err := grants.load(ctx, tKey)
	require.NoError(t, err)
	stale, _, err := leases.Acquire(ctx, partition, "main-0", time.Millisecond, snap.Generation)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	_, _, err = leases.Acquire(ctx, partition, "main-1", time.Minute, snap.Generation)
	require.NoError(t, err)
	err = leases.Commit(ctx, stale, func(ctx context.Context, tx pgx.Tx) error {
		_, err := grants.writeRefreshed(ctx, tx, snap, TokenResponse{AccessToken: "at-stale", RefreshToken: "rt-stale"})
		return err
	})
	require.ErrorIs(t, err, refresh.ErrStaleFence)
	after, err := grants.load(ctx, tKey)
	require.NoError(t, err)
	require.Equal(t, "at-0", after.AccessToken)
	require.Equal(t, snap.Generation, after.Generation)
}

// A refused refresh token invalidates the grant (wiped, epoch bumped,
// audited) and asks for consent again; no further refresh is attempted.
func TestInvalidGrantRequiresConsentAgain(t *testing.T) {
	config := newOAuthDatabase(t)
	provider, srv := newRotatingProvider(t, tRedirect)
	provider.refreshToken = "rt-elsewhere"
	source := replica(t, config, jiraClient(t, srv.URL, tRedirect), "main-0")
	admin := openPool(t, config)
	seedGrant(t, newTestGrants(t, admin))

	_, err := source.AccessToken(context.Background(), tKey)
	require.ErrorIs(t, err, ErrConsentRequired)
	_, err = source.AccessToken(context.Background(), tKey)
	require.ErrorIs(t, err, ErrConsentRequired)
	require.EqualValues(t, 1, provider.refreshCalls.Load())
	var state string
	var epoch int64
	var access, refreshSeal []byte
	require.NoError(t, admin.QueryRow(context.Background(), `
SELECT state, revocation_epoch, access_token_ciphertext, refresh_token_ciphertext
  FROM elitea_identity.provider_oauth_grants`).Scan(&state, &epoch, &access, &refreshSeal))
	require.Equal(t, StateInvalid, state)
	require.EqualValues(t, 1, epoch)
	require.Nil(t, access)
	require.Nil(t, refreshSeal)
	require.Equal(t, 2, countAudit(t, admin, "provider_oauth_grant"))
}

func countAudit(t *testing.T, pool *pgxpool.Pool, resourceType string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event::text LIKE '%' || $1 || '%'`,
		resourceType).Scan(&n))
	return n
}

type fixedTargets map[string]credentialgrant.TargetBinding

func (f fixedTargets) TargetBinding(_ context.Context, tenant int64, id string) (credentialgrant.TargetBinding, error) {
	t, ok := f[id]
	if !ok || t.TenantProjectID != tenant {
		return credentialgrant.TargetBinding{}, credentialgrant.ErrTargetNotFound
	}
	return t, nil
}

type fixedBindings map[string]agentidentity.AgentIdentityBinding

func (f fixedBindings) GetUsable(_ context.Context, id string) (agentidentity.AgentIdentityBinding, error) {
	b, ok := f[id]
	if !ok {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	return b, nil
}

func newTestConsent(t *testing.T, pool *pgxpool.Pool, client *Client) *Consent {
	t.Helper()
	c, err := NewConsent(ConsentConfig{
		Grants: newTestGrants(t, pool), Clients: []*Client{client},
		Targets:  fixedTargets{tTarget: {ID: tTarget, TenantProjectID: tTenant, ProviderType: "jira", TargetIdentity: "https://acme.atlassian.net"}},
		Bindings: fixedBindings{tBinding: {ID: tBinding, TenantProjectID: tTenant, State: agentidentity.BindingActive}},
	})
	require.NoError(t, err)
	return c
}

func asUser(user int64) Authorize {
	return func(context.Context, int64) (int64, error) { return user, nil }
}

var tStart = StartRequest{UserID: tUser, Provider: "jira", TenantProjectID: tTenant, AgentBindingID: tBinding, TargetBindingID: tTarget}

// Acceptance: the consent flow stores the grant; a bad, expired, reused or
// foreign state is refused; the verifier and the exact redirect URI reach
// the token endpoint; no token reaches a log or an error.
func TestConsentFlow(t *testing.T) {
	config := newOAuthDatabase(t)
	pool := openPool(t, config)
	provider, srv := newRotatingProvider(t, tRedirect)
	client := jiraClient(t, srv.URL, tRedirect)
	consent := newTestConsent(t, pool, client)
	ctx := context.Background()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var errorTexts []string
	note := func(err error) error {
		if err != nil {
			errorTexts = append(errorTexts, err.Error())
		}
		return err
	}

	t.Run("start refuses a foreign target, a binding elsewhere and an unconfigured provider", func(t *testing.T) {
		r := tStart
		r.TargetBindingID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f99"
		_, err := consent.Start(ctx, r)
		require.ErrorIs(t, note(err), ErrTargetNotFound)
		r = tStart
		r.TenantProjectID = 8
		_, err = consent.Start(ctx, r)
		require.ErrorIs(t, note(err), ErrTargetNotFound)
		r = tStart
		r.Provider = "aha"
		_, err = consent.Start(ctx, r)
		require.ErrorIs(t, note(err), ErrProviderNotConfigured)
	})

	t.Run("happy path, then the state is single use", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		require.True(t, started.ExpiresAt.After(time.Now()))
		state := provider.authorize(t, started.AuthorizationURL, "code-1")
		result, err := consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-1"}, asUser(tUser))
		require.NoError(t, err)
		require.Equal(t, tTenant, result.TenantProjectID)
		require.Equal(t, tBinding, result.AgentBindingID)
		require.Equal(t, tTarget, result.TargetBindingID)

		snap, err := consent.grants.load(ctx, tKey)
		require.NoError(t, err)
		require.Equal(t, "at-consent", snap.AccessToken)
		require.Equal(t, result.GrantID, snap.ID)

		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-1"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrStateInvalid, "a reused state was accepted")
		require.EqualValues(t, 1, provider.exchanges.Load())
		require.Equal(t, 1, countAudit(t, pool, "provider_oauth_grant.consent"))
	})

	t.Run("an unknown state is refused", func(t *testing.T) {
		_, err := consent.Callback(ctx, CallbackRequest{Provider: "jira", State: "not-a-state", Code: "c"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrStateInvalid)
	})

	t.Run("an expired state is refused", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-2")
		consent.now = func() time.Time { return time.Now().Add(DefaultStateTTL + time.Second) }
		defer func() { consent.now = time.Now }()
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-2"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrStateInvalid)
	})

	t.Run("another user's state is refused and consumed", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-3")
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-3"}, asUser(tUser+1))
		require.ErrorIs(t, note(err), ErrStateInvalid)
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-3"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrStateInvalid)
	})

	t.Run("a state for another provider is refused", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-4")
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "confluence", State: state, Code: "code-4"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrProviderNotConfigured)
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-4"}, asUser(tUser))
		require.NoError(t, err, "a refused provider must not consume another provider's state")
	})

	t.Run("a declined consent is refused", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-5")
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, ProviderError: "access_denied"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrConsentDenied)
	})

	t.Run("a lost permission is refused before the code is exchanged", func(t *testing.T) {
		before := provider.exchanges.Load()
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-6")
		denied := errors.New("insufficient permissions")
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-6"},
			func(context.Context, int64) (int64, error) { return 0, denied })
		require.ErrorIs(t, note(err), denied)
		require.Equal(t, before, provider.exchanges.Load())
	})

	t.Run("the exchange needs the matching verifier", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-7")
		// The provider holds another start's challenge for this code.
		other, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		provider.authorize(t, other.AuthorizationURL, "code-7")
		_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-7"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrInvalidGrant)
	})

	t.Run("a redirect URI changed since the start is refused", func(t *testing.T) {
		started, err := consent.Start(ctx, tStart)
		require.NoError(t, err)
		state := provider.authorize(t, started.AuthorizationURL, "code-8")
		moved := newTestConsent(t, pool, jiraClient(t, srv.URL, "http://127.0.0.1:9/other/api/v2/credential_consent/jira:callback"))
		_, err = moved.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-8"}, asUser(tUser))
		require.ErrorIs(t, note(err), ErrStateInvalid)
	})

	t.Run("no token, code or verifier in logs or errors", func(t *testing.T) {
		var verifierSeals int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.provider_oauth_consent_states`).Scan(&verifierSeals))
		require.Positive(t, verifierSeals)
		text := logs.String() + strings.Join(errorTexts, "\n")
		for _, secret := range []string{"at-consent", "rt-0", "client-secret", "code-1", "code-7"} {
			require.NotContains(t, text, secret)
		}
		// The table holds digests and ciphertext only.
		var plaintext int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_identity.provider_oauth_grants
 WHERE position(convert_to('at-consent', 'UTF8') in access_token_ciphertext) > 0`).Scan(&plaintext))
		require.Zero(t, plaintext)
	})
}

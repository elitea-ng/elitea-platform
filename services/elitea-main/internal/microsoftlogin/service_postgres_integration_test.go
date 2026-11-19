package microsoftlogin

// The sign-in against a real PostgreSQL: the shipped migration corpus (shared 0256
// included) applied to a template database, then Start, Callback and Redeem over the
// real tables and an Entra fixture. CI-only: skipped without ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	urlEnv    = "ELITEA_TEST_DATABASE_URL"
	bootstrap = "../infra/db/migrations/001_initial.sql"

	projectID    int64 = 7
	starter      int64 = 11
	otherUser    int64 = 12
	credentialID       = "aaaaaaaa-0000-4000-8000-000000000001"
	desktopPort        = 49321
)

var (
	templateOnce sync.Once
	template     string
	templateErr  error
)

func newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(urlEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the Microsoft sign-in integration test", urlEnv)
	}
	templateOnce.Do(func() {
		seed, err := os.ReadFile(bootstrap)
		if err != nil {
			templateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			templateErr = err
			return
		}
		defer admin.Close()
		template, templateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{Files: platformmigrations.Files, Seed: string(seed)})
	})
	require.NoError(t, templateErr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_mslogin_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, template, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *Service
	fx     *fixture
	creds  *fakeCredentials
	apps   *fakeApps
	clock  *clock
	logs   *syncBuffer
	custom Credential
}

// syncBuffer collects every log line of the service, to grep for planted values.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func customOutlook() Credential {
	return Credential{ID: credentialID, Type: "outlook", Data: map[string]any{
		"client_id": fixtureClient, "client_secret": fixtureSecret,
		"oauth_discovery_endpoint": "https://login.microsoftonline.com/" + fixtureTenant,
		"scopes":                   []any{"Mail.Read"}, "auto_refresh_token": true,
	}}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := newDatabase(t)
	r := &rig{t: t, pool: pool, fx: newFixture(), creds: &fakeCredentials{}, apps: &fakeApps{customAllow: true},
		clock: &clock{t: time.Now().UTC()}, logs: &syncBuffer{}}
	r.custom = customOutlook()
	r.creds.put(projectID, r.custom)
	svc, err := New(Config{
		Pool: pool, MasterKey: bytes.Repeat([]byte{0x42}, 32), Credentials: r.creds, Apps: r.apps, Do: r.fx.do,
		Origin: "https://elitea.example.com", Now: r.clock.now,
		Logger: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	require.NoError(t, err)
	r.svc = svc
	return r
}

func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (r *rig) start(kind string, mutate ...func(*StartRequest)) (StartResult, error) {
	req := StartRequest{UserID: starter, ProjectID: projectID, CredentialID: credentialID, ClientKind: kind}
	if kind != ClientWeb {
		_, req.ClientChallenge = pkce(r.t)
	}
	switch kind {
	case ClientDesktop:
		req.LoopbackPort = desktopPort
	case ClientMobile:
		req.ReturnScheme = "elitea"
	}
	for _, m := range mutate {
		m(&req)
	}
	return r.svc.Start(context.Background(), req)
}

// finish plays the browser (authorize at Entra) and the redirect to the callback.
func (r *rig) finish(res StartResult) Outcome {
	r.t.Helper()
	state, code, err := r.fx.authorize(res.AuthorizationURL)
	require.NoError(r.t, err)
	return r.svc.Callback(context.Background(), CallbackParams{State: state, Code: code})
}

func (r *rig) redeem(o Outcome, user int64, verifier string) (Redeemed, error) {
	return r.svc.Redeem(context.Background(), RedeemRequest{
		UserID: user, ProjectID: projectID, Handoff: o.Handoff, State: o.State, ClientVerifier: verifier,
	})
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM elitea_identity."+table).Scan(&n))
	return n
}

func TestAFullSignInWorksForEveryClientKind(t *testing.T) {
	verifier, _ := pkce(t)
	for _, tc := range []struct {
		kind, verifier, returnPrefix string
	}{
		{ClientWeb, "", ""},
		{ClientDesktop, verifier, fmt.Sprintf("http://127.0.0.1:%d/oauth/microsoft?", desktopPort)},
		{ClientMobile, verifier, "elitea://oauth/microsoft?"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r := newRig(t)
			res, err := r.start(tc.kind)
			require.NoError(t, err)
			require.Equal(t, AppCustom, res.App)

			outcome := r.finish(res)
			require.True(t, outcome.Known)
			require.Empty(t, outcome.ErrorCode)
			require.NotEmpty(t, outcome.Handoff)
			require.Equal(t, res.State, outcome.State)
			require.Equal(t, tc.kind, outcome.ClientKind)
			if tc.returnPrefix != "" {
				back := outcome.ReturnURL()
				require.True(t, strings.HasPrefix(back, tc.returnPrefix), back)
				// The handoff code and the state are the only values in the URL: no
				// token, no authorization code, no secret.
				for _, forbidden := range []string{"PLANTED", "access", "secret", "code="} {
					require.NotContains(t, back, forbidden)
				}
			}

			got, err := r.redeem(outcome, starter, tc.verifier)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(got.AccessToken, "access-token-"))
			require.Equal(t, fixtureTenant, got.TenantID)
			require.Equal(t, "Alex Example", got.AccountName)
			require.Equal(t, credentialID, got.CredentialID)
			require.Equal(t, "outlook", got.CredentialType)
			require.Contains(t, got.Scope, "Mail.Read")
			require.Contains(t, got.Scope, "offline_access")
			require.WithinDuration(t, r.clock.now().Add(3599*time.Second), got.ExpiresAt, 2*time.Second)

			// Single use: the redemption deleted the row.
			_, err = r.redeem(outcome, starter, tc.verifier)
			require.ErrorIs(t, err, ErrHandoffInvalid)
			require.Zero(t, count(t, r.pool, "microsoft_login_handoffs"))
			// The client secret went to the authority's token endpoint and nowhere else,
			// and nothing but Entra and Graph was called.
			require.Equal(t, []string{"login.microsoftonline.com/" + fixtureTenant + "/oauth2/v2.0/token"}, r.fx.secretHosts)
			require.NoError(t, r.fx.onlyEntraAndGraph())
		})
	}
}

func TestALoginCSRFEndsInAHandoffNobodyCanRedeem(t *testing.T) {
	verifier, _ := pkce(t)
	t.Run("the victim who finishes the attacker's link cannot redeem it as the attacker", func(t *testing.T) {
		r := newRig(t)
		// The attacker (starter) starts, and sends the victim the authorization URL.
		res, err := r.start(ClientWeb)
		require.NoError(t, err)
		// The victim signs in with THEIR Microsoft account: the callback runs in the
		// victim's browser and parks the victim's token for the starter.
		outcome := r.finish(res)
		require.NotEmpty(t, outcome.Handoff)
		require.Equal(t, 1, count(t, r.pool, "microsoft_login_handoffs"))
		// The victim's session is another user: refused, and the refusal deletes nothing,
		// so a stranger cannot destroy the starter's sign-in.
		_, err = r.redeem(outcome, otherUser, "")
		require.ErrorIs(t, err, ErrHandoffInvalid)
		require.Equal(t, 1, count(t, r.pool, "microsoft_login_handoffs"))
		// Nobody redeems it as the attacker's credential owner either: the handoff parks
		// the victim's token for the starter only, and expires unredeemed.
		r.clock.advance(HandoffTTL + time.Second)
		_, err = r.redeem(outcome, starter, "")
		require.ErrorIs(t, err, ErrHandoffInvalid)
	})
	for _, tc := range []struct {
		name   string
		kind   string
		mutate func(*Outcome, *RedeemRequest)
	}{
		{"another user", ClientWeb, func(_ *Outcome, q *RedeemRequest) { q.UserID = otherUser }},
		{"another project", ClientWeb, func(_ *Outcome, q *RedeemRequest) { q.ProjectID = projectID + 1 }},
		{"another state", ClientWeb, func(_ *Outcome, q *RedeemRequest) { q.State = "some-other-state" }},
		{"a verifier on a web sign-in", ClientWeb, func(_ *Outcome, q *RedeemRequest) { q.ClientVerifier = verifier }},
		{"no verifier on desktop", ClientDesktop, func(_ *Outcome, q *RedeemRequest) { q.ClientVerifier = "" }},
		{"the wrong verifier on mobile", ClientMobile, func(_ *Outcome, q *RedeemRequest) { q.ClientVerifier = "not-the-verifier" }},
		{"another user with the right verifier", ClientDesktop, func(_ *Outcome, q *RedeemRequest) { q.UserID = otherUser }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			res, err := r.start(tc.kind)
			require.NoError(t, err)
			outcome := r.finish(res)
			req := RedeemRequest{UserID: starter, ProjectID: projectID, Handoff: outcome.Handoff, State: outcome.State}
			if tc.kind != ClientWeb {
				req.ClientVerifier = verifier
			}
			tc.mutate(&outcome, &req)
			_, err = r.svc.Redeem(context.Background(), req)
			require.ErrorIs(t, err, ErrHandoffInvalid)
			// A wrong proof deletes nothing: the starter, with the right one, still gets it.
			require.Equal(t, 1, count(t, r.pool, "microsoft_login_handoffs"))
			good := RedeemRequest{UserID: starter, ProjectID: projectID, Handoff: outcome.Handoff, State: outcome.State}
			if tc.kind != ClientWeb {
				good.ClientVerifier = verifier
			}
			got, err := r.svc.Redeem(context.Background(), good)
			require.NoError(t, err)
			require.NotEmpty(t, got.AccessToken)
			require.Zero(t, count(t, r.pool, "microsoft_login_handoffs"))
		})
	}
	t.Run("an expired handoff", func(t *testing.T) {
		r := newRig(t)
		res, err := r.start(ClientWeb)
		require.NoError(t, err)
		outcome := r.finish(res)
		r.clock.advance(HandoffTTL + time.Second)
		_, err = r.redeem(outcome, starter, "")
		require.ErrorIs(t, err, ErrHandoffInvalid)
		// The next finished sign-in sweeps it: a sealed token is not kept past its minute.
		again, err := r.start(ClientWeb)
		require.NoError(t, err)
		require.NotEmpty(t, r.finish(again).Handoff)
		require.Equal(t, 1, count(t, r.pool, "microsoft_login_handoffs"))
	})
	t.Run("a handoff moved to another user's row does not open", func(t *testing.T) {
		r := newRig(t)
		res, err := r.start(ClientWeb)
		require.NoError(t, err)
		outcome := r.finish(res)
		// A database write that re-points the row at another user: the sealed token
		// is bound to the starter, so even a redemption by that user finds nothing.
		_, err = r.pool.Exec(context.Background(), `UPDATE elitea_identity.microsoft_login_handoffs SET user_id = $1`, otherUser)
		require.NoError(t, err)
		_, err = r.redeem(outcome, otherUser, "")
		require.ErrorIs(t, err, ErrHandoffInvalid)
	})
}

func TestAStateIsSingleUseAndExpires(t *testing.T) {
	r := newRig(t)
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	state, code, err := r.fx.authorize(res.AuthorizationURL)
	require.NoError(t, err)
	first := r.svc.Callback(context.Background(), CallbackParams{State: state, Code: code})
	require.NotEmpty(t, first.Handoff)
	replay := r.svc.Callback(context.Background(), CallbackParams{State: state, Code: code})
	require.False(t, replay.Known, "a replayed callback is unknown")
	require.Empty(t, replay.Handoff)

	res, err = r.start(ClientWeb)
	require.NoError(t, err)
	state, code, err = r.fx.authorize(res.AuthorizationURL)
	require.NoError(t, err)
	r.clock.advance(StateTTL + time.Second)
	require.False(t, r.svc.Callback(context.Background(), CallbackParams{State: state, Code: code}).Known)

	require.False(t, r.svc.Callback(context.Background(), CallbackParams{State: "never-issued", Code: "c"}).Known)
	require.False(t, r.svc.Callback(context.Background(), CallbackParams{}).Known)
}

func TestAHostileDiscoveryEndpointCannotRedirectTheCodeOrTheSecret(t *testing.T) {
	for _, endpoint := range []string{
		"https://evil.example/" + fixtureTenant,
		"https://login.microsoftonline.com.evil.example/" + fixtureTenant,
		"http://login.microsoftonline.com/" + fixtureTenant,
		"https://login.microsoftonline.com:8443/" + fixtureTenant,
		"https://user@login.microsoftonline.com/" + fixtureTenant,
		"https://login.microsoftonline.com/" + fixtureTenant + "/extra/path",
	} {
		t.Run(endpoint, func(t *testing.T) {
			r := newRig(t)
			hostile := customOutlook()
			hostile.Data["oauth_discovery_endpoint"] = endpoint
			r.creds.put(projectID, hostile)
			_, err := r.start(ClientWeb)
			var refusal *Refusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, CodeAuthorityInvalid, refusal.Code)
			require.Empty(t, r.fx.requests)
			require.Zero(t, count(t, r.pool, "microsoft_login_states"))
		})
	}
	t.Run("a token endpoint of the credential's own is never used", func(t *testing.T) {
		r := newRig(t)
		sneaky := customOutlook()
		sneaky.Data["token_endpoint"] = "https://evil.example/token"
		sneaky.Data["authorization_endpoint"] = "https://evil.example/authorize"
		r.creds.put(projectID, sneaky)
		res, err := r.start(ClientWeb)
		require.NoError(t, err)
		require.NotContains(t, res.AuthorizationURL, "evil.example")
		outcome := r.finish(res)
		require.NotEmpty(t, outcome.Handoff)
		require.NoError(t, r.fx.onlyEntraAndGraph())
		require.Equal(t, []string{"login.microsoftonline.com/" + fixtureTenant + "/oauth2/v2.0/token"}, r.fx.secretHosts)
	})
}

func TestCustomAppOffRefusesTheSignInStartAndTheCallback(t *testing.T) {
	r := newRig(t)
	started, err := r.start(ClientWeb)
	require.NoError(t, err)

	// An administrator turns Custom app off while the person is at Microsoft.
	r.apps.set(func(a *fakeApps) { a.customAllow = false })
	outcome := r.finish(started)
	require.True(t, outcome.Known)
	require.Equal(t, CodeCustomAppsDisabled, outcome.ErrorCode)
	require.Empty(t, outcome.Handoff)
	require.Zero(t, count(t, r.pool, "microsoft_login_handoffs"))

	// A credential saved around the form's switch cannot start a sign-in either.
	_, err = r.start(ClientWeb)
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, CodeCustomAppsDisabled, refusal.Code)
	require.Zero(t, count(t, r.pool, "microsoft_login_states")-1, "only the first start left a state")

	// A Default app credential is not affected by the switch.
	r.apps.set(func(a *fakeApps) { a.resolved = defaultApp() })
	r.creds.put(projectID, defaultAppOutlook())
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	require.NotEqual(t, AppCustom, res.App)
}

func defaultApp() microsoftapps.Resolved {
	return microsoftapps.Resolved{
		AppID: "bbbbbbbb-0000-4000-8000-000000000002", ClientID: fixtureClient,
		Authority: "https://login.microsoftonline.com/" + fixtureTenant,
		Secret:    microsoftapps.NewSecret(fixtureSecret), AutoRefreshToken: true,
		Ceiling: []string{"Mail.Read", "Mail.Send"},
	}
}

func defaultAppOutlook() Credential {
	return Credential{ID: credentialID, Type: "outlook", Data: map[string]any{
		"oauth_app_id": "bbbbbbbb-0000-4000-8000-000000000002",
		"scopes":       []any{"Mail.Read", "Mail.ReadWrite"},
	}}
}

func TestADefaultAppSignInKeepsTheSecretInMainAndAppliesTheCeiling(t *testing.T) {
	r := newRig(t)
	r.apps.set(func(a *fakeApps) { a.resolved = defaultApp(); a.customAllow = false })
	r.creds.put(projectID, defaultAppOutlook())

	res, err := r.start(ClientDesktop)
	require.NoError(t, err)
	require.Equal(t, "bbbbbbbb-0000-4000-8000-000000000002", res.App)
	parsed, err := url.Parse(res.AuthorizationURL)
	require.NoError(t, err)
	scopes := strings.Fields(parsed.Query().Get("scope"))
	// Mail.ReadWrite is above the app's ceiling and is cut; the identity scopes ride along.
	require.ElementsMatch(t, []string{"offline_access", "Mail.Read", "openid", "profile"}, scopes)
	require.NotContains(t, res.AuthorizationURL, fixtureSecret)

	outcome := r.finish(res)
	require.NotEmpty(t, outcome.Handoff)
	verifier, _ := pkce(t)
	got, err := r.redeem(outcome, starter, verifier)
	require.NoError(t, err)
	require.Equal(t, "bbbbbbbb-0000-4000-8000-000000000002", got.App)
	// The registry was asked at the start AND at the callback, with this project.
	require.Len(t, r.apps.requests, 2)
	for _, req := range r.apps.requests {
		require.Equal(t, microsoftapps.Request{AppID: "bbbbbbbb-0000-4000-8000-000000000002", CredentialType: "outlook", ProjectID: projectID}, req)
	}
	require.Equal(t, []string{"login.microsoftonline.com/" + fixtureTenant + "/oauth2/v2.0/token"}, r.fx.secretHosts)
	// The secret is in no response and no log.
	require.NotContains(t, fmt.Sprintf("%+v %+v %v", res, got, outcome), fixtureSecret)
	require.NotContains(t, r.logs.String(), fixtureSecret)
}

func TestADefaultAppThatStopsBeingUsableEndsTheSignInWithItsCode(t *testing.T) {
	r := newRig(t)
	r.apps.set(func(a *fakeApps) { a.resolved = defaultApp() })
	r.creds.put(projectID, defaultAppOutlook())
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	r.apps.set(func(a *fakeApps) {
		a.resolved, a.err = microsoftapps.Resolved{}, &microsoftapps.ResolveError{Code: microsoftapps.CodeAppDisabled, Message: "disabled"}
	})
	outcome := r.finish(res)
	require.Equal(t, microsoftapps.CodeAppDisabled, outcome.ErrorCode)
	require.Empty(t, outcome.Handoff)
	// The refusal at the start carries the registry's stable code too.
	_, err = r.start(ClientWeb)
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, microsoftapps.CodeAppDisabled, refusal.Code)
}

func TestACredentialThatMovedToAnotherClientDuringTheSignInIsNotFinished(t *testing.T) {
	r := newRig(t)
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	moved := customOutlook()
	moved.Data["client_id"] = "44444444-4444-4444-4444-444444444444"
	r.creds.put(projectID, moved)
	outcome := r.finish(res)
	require.Equal(t, CodeCredentialChanged, outcome.ErrorCode)
	require.Empty(t, outcome.Handoff)
	require.False(t, r.fx.sawHost("login.microsoftonline.com") && len(r.fx.secretHosts) > 0, "the code was not exchanged")
}

func TestFailuresAtMicrosoftAreReportedToTheClientAndParkNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*rig)
		params func(state, code string) CallbackParams
		want   string
	}{
		{"declined", func(*rig) {}, func(s, _ string) CallbackParams { return CallbackParams{State: s, Error: "access_denied"} }, ErrDenied},
		{"another provider error", func(*rig) {}, func(s, _ string) CallbackParams { return CallbackParams{State: s, Error: "server_error"} }, ErrProvider},
		{"no code", func(*rig) {}, func(s, _ string) CallbackParams { return CallbackParams{State: s} }, ErrExchange},
		{"the code is refused", func(r *rig) { r.fx.failToken = true }, func(s, c string) CallbackParams { return CallbackParams{State: s, Code: c} }, ErrExchange},
		{"another tenant's person", func(r *rig) { r.fx.tid = "99999999-9999-9999-9999-999999999999" }, func(s, c string) CallbackParams { return CallbackParams{State: s, Code: c} }, ErrTenant},
		{"a token that lacks the permission", func(r *rig) { r.fx.forbidGraph = true }, func(s, c string) CallbackParams { return CallbackParams{State: s, Code: c} }, ErrPermission},
		{"a token Entra then rejects", func(r *rig) { r.fx.rejectGraph = true }, func(s, c string) CallbackParams { return CallbackParams{State: s, Code: c} }, ErrRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(r)
			res, err := r.start(ClientMobile)
			require.NoError(t, err)
			state, code, err := r.fx.authorize(res.AuthorizationURL)
			require.NoError(t, err)
			outcome := r.svc.Callback(context.Background(), tc.params(state, code))
			require.True(t, outcome.Known)
			require.Equal(t, tc.want, outcome.ErrorCode)
			require.Empty(t, outcome.Handoff)
			require.Zero(t, count(t, r.pool, "microsoft_login_handoffs"))
			// The mobile client is told by URL, with the code word and the state only.
			back := outcome.ReturnURL()
			require.True(t, strings.HasPrefix(back, "elitea://oauth/microsoft?"), back)
			require.Contains(t, back, "error="+tc.want)
			require.NotContains(t, back, "handoff=")
		})
	}
}

func TestATokenWithoutAnIdentityIsRefused(t *testing.T) {
	r := newRig(t)
	r.fx.oid = "not-a-guid"
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	outcome := r.finish(res)
	require.Equal(t, ErrIdentity, outcome.ErrorCode)
}

func TestSecretsNeverReachLogsOrTables(t *testing.T) {
	r := newRig(t)
	// Both a success and failures write logs; plant a failing exchange too.
	for _, kind := range []string{ClientWeb, ClientDesktop, ClientMobile} {
		res, err := r.start(kind)
		require.NoError(t, err)
		outcome := r.finish(res)
		verifier, _ := pkce(t)
		if kind == ClientWeb {
			verifier = ""
		}
		_, err = r.redeem(outcome, starter, verifier)
		require.NoError(t, err)
	}
	r.fx.failToken = true
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	require.Equal(t, ErrExchange, r.finish(res).ErrorCode)
	// A finished sign-in left in the table must hold no plaintext either.
	r.fx.failToken = false
	parked, err := r.start(ClientWeb)
	require.NoError(t, err)
	outcome := r.finish(parked)
	require.NotEmpty(t, outcome.Handoff)

	logs := r.logs.String()
	require.NotEmpty(t, logs, "the flow logs its outcomes")
	rows := ""
	for _, table := range []string{"microsoft_login_states", "microsoft_login_handoffs"} {
		var dump string
		require.NoError(t, r.pool.QueryRow(context.Background(),
			"SELECT coalesce(string_agg(t::text, E'\\n'), '') FROM elitea_identity."+table+" t").Scan(&dump))
		rows += dump
	}
	for _, planted := range []string{"PLANTED", fixtureSecret, refreshToken, outcome.Handoff, outcome.State} {
		require.NotContains(t, logs, planted, "a planted value reached the logs")
		require.NotContains(t, rows, planted, "a planted value reached a table in plaintext")
	}
	// The Entra error body is never logged, only its code and correlation id.
	require.NotContains(t, logs, "AADSTS")
	require.Contains(t, logs, "invalid_grant")
}

func TestAStartRefusesWhatItCannotBind(t *testing.T) {
	_, challenge := pkce(t)
	r := newRig(t)
	for _, tc := range []struct {
		name   string
		mutate func(*StartRequest)
		code   string
	}{
		{"an unknown client kind", func(q *StartRequest) { q.ClientKind = "tv" }, CodeClientKindInvalid},
		{"a web sign-in with a challenge", func(q *StartRequest) { q.ClientChallenge = challenge }, CodeClientKindInvalid},
		{"a desktop sign-in with no challenge", func(q *StartRequest) { q.ClientKind, q.ClientChallenge, q.LoopbackPort = ClientDesktop, "", 4000 }, CodeClientChallenge},
		{"a desktop sign-in on a privileged port", func(q *StartRequest) { q.ClientKind, q.ClientChallenge, q.LoopbackPort = ClientDesktop, challenge, 80 }, CodeClientReturnInvalid},
		{"a mobile sign-in on a script scheme", func(q *StartRequest) {
			q.ClientKind, q.ClientChallenge, q.ReturnScheme = ClientMobile, challenge, "javascript"
		}, CodeClientReturnInvalid},
		{"a mobile sign-in on a dialer scheme", func(q *StartRequest) {
			q.ClientKind, q.ClientChallenge, q.ReturnScheme = ClientMobile, challenge, "tel"
		}, CodeClientReturnInvalid},
		{"a mobile sign-in on https", func(q *StartRequest) {
			q.ClientKind, q.ClientChallenge, q.ReturnScheme = ClientMobile, challenge, "https"
		}, CodeClientReturnInvalid},
		{"a credential id that is not a uuid", func(q *StartRequest) { q.CredentialID = "1" }, CodeCredentialIDInvalid},
		{"a credential of another project", func(q *StartRequest) { q.ProjectID = projectID + 1 }, CodeCredentialNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := StartRequest{UserID: starter, ProjectID: projectID, CredentialID: credentialID, ClientKind: ClientWeb}
			tc.mutate(&req)
			_, err := r.svc.Start(context.Background(), req)
			var refusal *Refusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, tc.code, refusal.Code)
		})
	}
	require.Zero(t, count(t, r.pool, "microsoft_login_states"))
}

func TestOnePersonCannotHoldMoreThanTheCapOfOpenSignIns(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxLiveSignIns; i++ {
		_, err := r.start(ClientWeb)
		require.NoError(t, err)
	}
	_, err := r.start(ClientWeb)
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, CodeTooManySignIns, refusal.Code)
	// Another person is not affected, and expiry frees the slots.
	_, err = r.start(ClientWeb, func(q *StartRequest) { q.UserID = otherUser })
	require.NoError(t, err)
	r.clock.advance(StateTTL + time.Second)
	_, err = r.start(ClientWeb)
	require.NoError(t, err)
}

func TestAStartPrunesWhatExpiredAnHourAgo(t *testing.T) {
	r := newRig(t)
	res, err := r.start(ClientWeb)
	require.NoError(t, err)
	outcome := r.finish(res)
	require.NotEmpty(t, outcome.Handoff)
	require.Equal(t, 1, count(t, r.pool, "microsoft_login_handoffs"))
	r.clock.advance(2 * time.Hour)
	_, err = r.start(ClientWeb)
	require.NoError(t, err)
	require.Zero(t, count(t, r.pool, "microsoft_login_handoffs"))
	require.Equal(t, 1, count(t, r.pool, "microsoft_login_states"))
}

func TestTheStateRowShapeIsEnforcedByTheDatabase(t *testing.T) {
	r := newRig(t)
	// A desktop state with no challenge, or a web state with a port, cannot exist.
	_, err := r.pool.Exec(context.Background(), `
INSERT INTO elitea_identity.microsoft_login_states
 (state_digest, user_id, tenant_project_id, credential_id, credential_type, app_ref, client_kind,
  client_id_fingerprint, redirect_uri, authority, scope, verifier_ciphertext, key_label, expires_at)
VALUES (decode(repeat('ab', 32), 'hex'), 1, 1, gen_random_uuid(), 'outlook', 'custom', 'desktop',
  repeat('a', 64), 'https://x/y', 'https://login.microsoftonline.com/t', 's', '\x00', 'l', now())`)
	require.Error(t, err)
}

func TestParallelStartsCannotExceedThePerPersonCap(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, refused := 0, 0
	for i := 0; i < 3*MaxLiveSignIns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.start(ClientWeb)
			mu.Lock()
			defer mu.Unlock()
			var refusal *Refusal
			switch {
			case err == nil:
				succeeded++
			case errors.As(err, &refusal) && refusal.Code == CodeTooManySignIns:
				refused++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, MaxLiveSignIns, succeeded)
	require.Equal(t, 2*MaxLiveSignIns, refused)
	require.Equal(t, MaxLiveSignIns, count(t, r.pool, "microsoft_login_states"))
}

func TestACredentialWhoseIdentityReadCannotBeBuiltIsRefusedAtTheStart(t *testing.T) {
	r := newRig(t)
	bad := Credential{ID: credentialID, Type: "dataverse", Data: map[string]any{
		"client_id": fixtureClient, "client_secret": fixtureSecret, "tenant_id": fixtureTenant,
		"url": "https://evil.example.com",
	}}
	r.creds.put(projectID, bad)
	_, err := r.start(ClientWeb)
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, CodeScopesInvalid, refusal.Code, "a Dataverse scope is derived from the org URL")
	badMailbox := customOutlook()
	badMailbox.Data["mailbox"] = "../../etc/passwd"
	r.creds.put(projectID, badMailbox)
	_, err = r.start(ClientWeb)
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, CodeClientInvalid, refusal.Code)
	require.Zero(t, count(t, r.pool, "microsoft_login_states"))
}

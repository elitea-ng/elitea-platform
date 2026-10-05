package nativeauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// The whole flow with a real Auth middleware, a real session cookie and the
// real tables: authorize -> sign-in bounce -> consent -> code -> tokens ->
// Bearer call -> refresh -> revoke -> refused.
func TestNativeFlowEndToEnd(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := newStack(t)
	userID := s.seedUser("alex@example.test")

	// Without a session, continue bounces to the deployment's own sign-in
	// with a constant same-origin target.
	start := s.do(http.MethodGet, authorizeQuery(nil), nil)
	if start.Code != http.StatusFound || !strings.HasPrefix(start.Header().Get("Location"), nativeapi.ContinuePath+"?request=") {
		t.Fatalf("authorize = %d %q", start.Code, start.Header().Get("Location"))
	}
	binder := binderFrom(t, start)
	if binder.HttpOnly != true || binder.SameSite != http.SameSiteLaxMode {
		t.Fatalf("binder cookie attributes = %+v", binder)
	}
	bounce := s.do(http.MethodGet, start.Header().Get("Location"), nil, withCookies(binder))
	wantBounce := "/auth/login?target_to=" + url.QueryEscape(start.Header().Get("Location"))
	if bounce.Code != http.StatusFound || bounce.Header().Get("Location") != wantBounce {
		t.Fatalf("unauthenticated continue = %d %q, want %q", bounce.Code, bounce.Header().Get("Location"), wantBounce)
	}

	code := s.signIn(userID, "alex@example.test")
	pair := s.exchange(code)
	if !strings.HasPrefix(pair.AccessToken, domain.PrefixAccessToken) ||
		!strings.HasPrefix(pair.RefreshToken, domain.PrefixRefreshToken) || pair.DeviceID == "" {
		t.Fatalf("pair = %+v", pair)
	}

	// The anchor: a real auth_core__token row with uuid NULL that no PAT
	// reader shows.
	anchor := s.anchorOf(pair.DeviceID)
	var uuid *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT uuid FROM public.auth_core__token WHERE id = $1`, anchor).Scan(&uuid); err != nil || uuid != nil {
		t.Fatalf("anchor uuid = %v (%v), want NULL", uuid, err)
	}
	queries := sqlcgen.New(s.pool)
	listed, err := queries.ListOwnedPATs(context.Background(), int32(userID))
	if err != nil || len(listed) != 0 {
		t.Fatalf("ListOwnedPATs = %v (%v); the device anchor must not be listed as a key", listed, err)
	}

	// A Bearer call with the native access token: the principal is shaped
	// exactly like a PAT principal, anchored on the token row.
	who := s.whoami(pair.AccessToken)
	if who.Code != http.StatusOK {
		t.Fatalf("whoami = %d %s", who.Code, who.Body.String())
	}
	var principal auth.User
	_ = json.Unmarshal(who.Body.Bytes(), &principal)
	if principal.AuthType != "token" || principal.UserID != itoa(userID) || principal.TokenID != itoa(anchor) {
		t.Fatalf("principal = %+v, want token principal on anchor %d", principal, anchor)
	}

	// Refresh: a new pair; the old access token keeps working until it
	// expires; the generation moved.
	refreshed, next := s.refresh(pair.RefreshToken)
	if refreshed.Code != http.StatusOK || next.RefreshToken == pair.RefreshToken || next.DeviceID != pair.DeviceID {
		t.Fatalf("refresh = %d %s", refreshed.Code, refreshed.Body.String())
	}
	if s.whoami(pair.AccessToken).Code != http.StatusOK || s.whoami(next.AccessToken).Code != http.StatusOK {
		t.Fatal("both access tokens of a live family must work until they expire")
	}
	var generations int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT max(generation) FROM elitea_auth.native_refresh_tokens WHERE session_id = $1`, pair.DeviceID).Scan(&generations)
	if generations != 2 {
		t.Fatalf("generation = %d, want 2", generations)
	}

	// Revoke with the ACCESS token revokes the whole family.
	revoke := s.do(http.MethodPost, nativeapi.RevokePath, url.Values{"token": {next.AccessToken}, "client_id": {testClientID}})
	if revoke.Code != http.StatusOK || revoke.Body.Len() != 0 {
		t.Fatalf("revoke = %d %q", revoke.Code, revoke.Body.String())
	}
	state := s.family(pair.DeviceID)
	if reason(state) != domain.ReasonSignedOut || state.tokenID != nil || s.anchorExists(anchor) {
		t.Fatalf("after revoke: %+v anchor=%v", state, s.anchorExists(anchor))
	}
	if cut := s.whoami(next.AccessToken); cut.Code != http.StatusUnauthorized ||
		!strings.Contains(cut.Body.String(), `"error":"device_revoked"`) {
		t.Fatalf("whoami after revoke = %d %s, want 401 device_revoked", cut.Code, cut.Body.String())
	}
	if refused, body := s.refresh(next.RefreshToken); refused.Code != http.StatusUnauthorized || body.Error != "device_revoked" {
		t.Fatalf("refresh after revoke = %d %s, want 401 device_revoked", refused.Code, refused.Body.String())
	}

	// Audit: exchange recorded; a successful refresh is NOT.
	actions := s.audit.actions()
	signedIn := 0
	for _, action := range actions {
		if strings.HasPrefix(action, "Native device signed in") {
			signedIn++
		}
		if strings.Contains(action, "refresh") {
			t.Fatalf("a successful refresh wrote an audit row: %q", actions)
		}
	}
	if signedIn != 1 {
		t.Fatalf("audit actions = %q, want exactly one sign-in row", actions)
	}

	// Log hygiene: no credential, code, handle or binder reached a log line.
	for _, secret := range []string{"elnat_", "elnrt_", "elnac_", binder.Value, pair.AccessToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log output carries credential material %q", secret[:6])
		}
	}
}

func itoa(v int64) string { return strconvFormat(v) }

func TestNativeSessionSkipsSignInWhenAlreadySignedIn(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("signed@example.test")
	// signIn presents the session cookie on the first continue: no bounce.
	_ = s.exchange(s.signIn(userID, "signed@example.test"))
}

func TestNativeCodeIsSingleUseAndReplayRevokesTheFamily(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("replay@example.test")
	code := s.signIn(userID, "replay@example.test")
	pair := s.exchange(code)

	replayed, body := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"client_id": {testClientID}, "code_verifier": {testVerifier},
	})
	if replayed.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
		t.Fatalf("replay = %d %s", replayed.Code, replayed.Body.String())
	}
	// Row state AFTER the response: the revocation committed.
	state := s.family(pair.DeviceID)
	if reason(state) != domain.ReasonCodeReplay || state.tokenID != nil {
		t.Fatalf("family after replay = %+v", state)
	}
	if s.whoami(pair.AccessToken).Code != http.StatusUnauthorized {
		t.Fatal("the family a replayed code produced must stop authenticating")
	}
	found := false
	for _, action := range s.audit.actions() {
		found = found || strings.Contains(action, "authorization code replayed")
	}
	if !found {
		t.Fatalf("no replay audit row in %q", s.audit.actions())
	}
}

func TestNativeCodeExpiresAfterSixtySeconds(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("slow@example.test")
	code := s.signIn(userID, "slow@example.test")
	s.advance(domain.CodeTTL)
	recorder, body := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"client_id": {testClientID}, "code_verifier": {testVerifier},
	})
	if recorder.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
		t.Fatalf("expired code = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestNativeExchangeChecksRedirectAndVerifier(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("pkce@example.test")
	for name, form := range map[string]url.Values{
		"redirect mismatch": {"redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {testVerifier}},
		"wrong verifier":    {"redirect_uri": {testRedirect}, "code_verifier": {strings.Repeat("a", 43)}},
	} {
		code := s.signIn(userID, "pkce@example.test")
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("client_id", testClientID)
		recorder, body := s.token(form)
		if recorder.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
			t.Fatalf("%s = %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
	// A malformed verifier is invalid_request, before any store read.
	recorder, body := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {"elnac_x"}, "redirect_uri": {testRedirect},
		"client_id": {testClientID}, "code_verifier": {"short"},
	})
	if recorder.Code != http.StatusBadRequest || body.Error != "invalid_request" {
		t.Fatalf("short verifier = %d %s", recorder.Code, recorder.Body.String())
	}
}

/* ── refresh rotation and the re-delivery window (decision 7) ───────────── */

func TestNativeRefreshRedeliveryWithinWindowReturnsTheSameSuccessor(t *testing.T) {
	s := newStack(t, withRedelivery(30*time.Second))
	userID := s.seedUser("lossy@example.test")
	pair := s.exchange(s.signIn(userID, "lossy@example.test"))

	first, successor := s.refresh(pair.RefreshToken)
	if first.Code != http.StatusOK {
		t.Fatalf("first refresh = %d %s", first.Code, first.Body.String())
	}
	// The response was "lost"; the client retries with the same token.
	s.advance(10 * time.Second)
	again, redelivered := s.refresh(pair.RefreshToken)
	if again.Code != http.StatusOK {
		t.Fatalf("re-delivery = %d %s", again.Code, again.Body.String())
	}
	if redelivered.AccessToken != successor.AccessToken || redelivered.RefreshToken != successor.RefreshToken {
		t.Fatal("re-delivery must return the SAME successor pair, not mint a new one")
	}
	state := s.family(pair.DeviceID)
	if state.revokedAt != nil || state.refreshTokens != 2 {
		t.Fatalf("after re-delivery: %+v (want live, exactly two generations)", state)
	}
	// The successor works and rotates normally.
	if s.whoami(redelivered.AccessToken).Code != http.StatusOK {
		t.Fatal("the redelivered access token must authenticate")
	}
	third, _ := s.refresh(redelivered.RefreshToken)
	if third.Code != http.StatusOK {
		t.Fatalf("successor refresh = %d %s", third.Code, third.Body.String())
	}
	// Once the successor itself was used, the old token is plain reuse.
	reused, _ := s.refresh(pair.RefreshToken)
	if reused.Code != http.StatusUnauthorized {
		t.Fatalf("old token after its successor was used = %d, want 401 device_revoked", reused.Code)
	}
	if reason(s.family(pair.DeviceID)) != domain.ReasonRefreshReuse {
		t.Fatalf("family = %+v, want revoked refresh_reuse", s.family(pair.DeviceID))
	}
}

func TestNativeRefreshReuseOutsideWindowRevokesTheFamily(t *testing.T) {
	s := newStack(t, withRedelivery(30*time.Second))
	userID := s.seedUser("thief@example.test")
	pair := s.exchange(s.signIn(userID, "thief@example.test"))
	anchor := s.anchorOf(pair.DeviceID)
	_, successor := s.refresh(pair.RefreshToken)

	s.advance(31 * time.Second)
	reused, body := s.refresh(pair.RefreshToken)
	if reused.Code != http.StatusUnauthorized || body.Error != "device_revoked" ||
		reused.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("reuse = %d %s", reused.Code, reused.Body.String())
	}
	// Row state after the response: revoked, anchor and binding gone. The
	// access token rows stay until they expire so the next API call answers
	// device_revoked; none of them authenticates.
	state := s.family(pair.DeviceID)
	if reason(state) != domain.ReasonRefreshReuse || state.tokenID != nil || s.anchorExists(anchor) {
		t.Fatalf("family after reuse = %+v anchor=%v", state, s.anchorExists(anchor))
	}
	// The newest access token is cut off: the validator says device revoked.
	if _, err := s.validator.ValidateToken(context.Background(), successor.AccessToken); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("validate newest token after reuse = %v", err)
	}
	// The API answers the ADR's flat device_revoked body, so the client
	// wipes rather than refreshing.
	cut := s.whoami(successor.AccessToken)
	if cut.Code != http.StatusUnauthorized || !strings.Contains(cut.Body.String(), `"error":"device_revoked"`) {
		t.Fatalf("newest access token after reuse = %d %s, want 401 device_revoked", cut.Code, cut.Body.String())
	}
	found := false
	for _, action := range s.audit.actions() {
		found = found || strings.Contains(action, "refresh token reused")
	}
	if !found {
		t.Fatalf("no reuse audit row in %q", s.audit.actions())
	}
}

func TestNativeConcurrentRefreshWithinWindowBothGetTheSamePair(t *testing.T) {
	s := newStack(t, withRedelivery(30*time.Second))
	userID := s.seedUser("race@example.test")
	pair := s.exchange(s.signIn(userID, "race@example.test"))
	results := concurrentRefresh(s, pair.RefreshToken, 2)
	for _, result := range results {
		if result.status != http.StatusOK {
			t.Fatalf("concurrent refresh statuses = %+v, want both 200 inside the window", results)
		}
	}
	if results[0].body.RefreshToken != results[1].body.RefreshToken ||
		results[0].body.AccessToken != results[1].body.AccessToken {
		t.Fatal("two racing refreshes inside the window must receive the identical successor")
	}
	if state := s.family(pair.DeviceID); state.revokedAt != nil || state.refreshTokens != 2 {
		t.Fatalf("after the race: %+v", state)
	}
}

func TestNativeConcurrentRefreshStrictModeRevokes(t *testing.T) {
	s := newStack(t, withRedelivery(0))
	userID := s.seedUser("strict@example.test")
	pair := s.exchange(s.signIn(userID, "strict@example.test"))
	results := concurrentRefresh(s, pair.RefreshToken, 2)
	ok := 0
	for _, result := range results {
		if result.status == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("strict mode statuses = %+v, want exactly one success", results)
	}
	if reason(s.family(pair.DeviceID)) != domain.ReasonRefreshReuse {
		t.Fatalf("strict mode family = %+v, want revoked refresh_reuse", s.family(pair.DeviceID))
	}
}

type refreshResult struct {
	status int
	body   tokenResponse
}

func concurrentRefresh(s *stack, refreshToken string, n int) []refreshResult {
	results := make([]refreshResult, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			recorder, body := s.refresh(refreshToken)
			results[index] = refreshResult{status: recorder.Code, body: body}
		}(i)
	}
	start.Done()
	done.Wait()
	return results
}

func TestNativeIdleExpiryAndAbsoluteCapRevoke(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		s := newStack(t)
		userID := s.seedUser("idle@example.test")
		pair := s.exchange(s.signIn(userID, "idle@example.test"))
		s.advance(domain.DefaultRefreshIdleTTL + time.Second)
		if refused, _ := s.refresh(pair.RefreshToken); refused.Code != http.StatusUnauthorized {
			t.Fatalf("idle refresh = %d", refused.Code)
		}
		if reason(s.family(pair.DeviceID)) != domain.ReasonExpired {
			t.Fatalf("idle family = %+v", s.family(pair.DeviceID))
		}
	})
	t.Run("absolute cap", func(t *testing.T) {
		s := newStack(t, withSessionMaxLifetime(24*time.Hour))
		userID := s.seedUser("cap@example.test")
		pair := s.exchange(s.signIn(userID, "cap@example.test"))
		s.advance(12 * time.Hour)
		ok, next := s.refresh(pair.RefreshToken)
		if ok.Code != http.StatusOK {
			t.Fatalf("refresh inside cap = %d", ok.Code)
		}
		s.advance(13 * time.Hour)
		if refused, _ := s.refresh(next.RefreshToken); refused.Code != http.StatusUnauthorized {
			t.Fatalf("refresh past cap = %d", refused.Code)
		}
		if reason(s.family(pair.DeviceID)) != domain.ReasonExpired {
			t.Fatalf("capped family = %+v", s.family(pair.DeviceID))
		}
	})
}

func TestNativeClientDisableAndRemovalRevoke(t *testing.T) {
	t.Run("disable in the DB layer revokes in the same transaction", func(t *testing.T) {
		s := newStack(t)
		userID := s.seedUser("disable@example.test")
		pair := s.exchange(s.signIn(userID, "disable@example.test"))
		client := fileClient()
		client.Enabled = false
		revoked, err := s.store.UpsertClient(context.Background(), client, userID)
		if err != nil || revoked != 1 {
			t.Fatalf("UpsertClient = %d, %v", revoked, err)
		}
		if reason(s.family(pair.DeviceID)) != domain.ReasonClientDisabled {
			t.Fatalf("family = %+v", s.family(pair.DeviceID))
		}
		// Re-enabling resurrects nothing.
		client.Enabled = true
		if _, err := s.store.UpsertClient(context.Background(), client, userID); err != nil {
			t.Fatal(err)
		}
		s.registry.Invalidate()
		if s.whoami(pair.AccessToken).Code != http.StatusUnauthorized {
			t.Fatal("re-enabling a client must not resurrect its devices")
		}
	})
	t.Run("removal from the file layer revokes at the next refresh", func(t *testing.T) {
		s := newStack(t)
		userID := s.seedUser("removed@example.test")
		pair := s.exchange(s.signIn(userID, "removed@example.test"))
		// A second client keeps the registry non-empty while ours is gone.
		other := domain.Client{ClientID: "dev.elitea.other", DisplayName: "Other", Enabled: true,
			RedirectURIs: []string{"dev.elitea.other:/cb"}, Source: domain.SourceFile}
		s.registry = domain.NewRegistry([]domain.Client{other}, s.pool)
		s.handler = nativeapi.New(nativeapi.Config{Registry: s.registry, Store: s.store, PublicOrigin: testOrigin})
		router := newBareRouter(s.handler)
		recorder := doOn(router, http.MethodPost, nativeapi.TokenPath, url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {pair.RefreshToken}, "client_id": {testClientID},
		})
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"device_revoked"`) {
			t.Fatalf("refresh of a removed client = %d %s", recorder.Code, recorder.Body.String())
		}
		if reason(s.family(pair.DeviceID)) != domain.ReasonClientRemoved {
			t.Fatalf("family = %+v", s.family(pair.DeviceID))
		}
	})
}

/* ── the validator and the principal re-check ───────────────────────────── */

func TestNativeValidatorRefusesInactiveOwners(t *testing.T) {
	t.Run("suspended", func(t *testing.T) {
		s := newStack(t)
		userID := s.seedUser("suspended@example.test")
		pair := s.exchange(s.signIn(userID, "suspended@example.test"))
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.validator.ValidateToken(context.Background(), pair.AccessToken); !errors.Is(err, auth.ErrDeviceRevoked) {
			t.Fatalf("suspended owner = %v, want ErrDeviceRevoked", err)
		}
		if refused, _ := s.refresh(pair.RefreshToken); refused.Code != http.StatusUnauthorized {
			t.Fatalf("refresh of a suspended owner = %d", refused.Code)
		}
		if reason(s.family(pair.DeviceID)) != domain.ReasonUserDeactivated {
			t.Fatalf("family = %+v", s.family(pair.DeviceID))
		}
	})
	t.Run("hard deleted", func(t *testing.T) {
		s := newStack(t)
		userID := s.seedUser("deleted@example.test")
		pair := s.exchange(s.signIn(userID, "deleted@example.test"))
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM public.auth_core__user WHERE id = $1`, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.validator.ValidateToken(context.Background(), pair.AccessToken); !errors.Is(err, auth.ErrDeviceRevoked) {
			t.Fatalf("deleted owner = %v, want ErrDeviceRevoked", err)
		}
		if s.family(pair.DeviceID).tokenID != nil {
			t.Fatal("the anchor foreign key must null token_id when the user (and its tokens) are deleted")
		}
	})
}

func TestNativePrincipalPassesThePrincipalRecheckUntilTheAnchorGoes(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("recheck@example.test")
	pair := s.exchange(s.signIn(userID, "recheck@example.test"))
	principal, err := s.validator.ValidateToken(context.Background(), pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	validator := authsvc.NewPrincipalValidator(s.pool)
	if _, err := validator.ValidatePrincipal(context.Background(), principal); err != nil {
		t.Fatalf("principal re-check of a live native principal = %v", err)
	}
	if _, err := s.store.RevokeByToken(context.Background(), pair.RefreshToken, testClientID); err != nil {
		t.Fatal(err)
	}
	// A forwarded (edge-validated) native principal is cut off by the anchor
	// deletion alone.
	if _, err := validator.ValidatePrincipal(context.Background(), principal); !errors.Is(err, auth.ErrPrincipalInactive) {
		t.Fatalf("principal re-check after revoke = %v, want ErrPrincipalInactive", err)
	}
}

func TestNativeAnchorCannotBeSignedAsPAT(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("anchor@example.test")
	_ = s.exchange(s.signIn(userID, "anchor@example.test"))
	_, err := sqlcgen.New(s.pool).GetActivePATForUser(context.Background(), int32(userID))
	if !errors.Is(err, errNoRows()) {
		t.Fatalf("GetActivePATForUser = %v; a user holding only a device anchor has no PAT to sign", err)
	}
	var active int64
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM public.auth_core__token WHERE user_id = $1 AND uuid IS NOT NULL`, userID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("PAT-shaped rows = %d (%v)", active, err)
	}
}

/* ── revoke, 404, loop guard, refused bearer ────────────────────────────── */

func TestNativeRevokeOfAnUnknownTokenIsAQuiet200(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("quiet@example.test")
	pair := s.exchange(s.signIn(userID, "quiet@example.test"))
	for _, token := range []string{"elnrt_unknown", "not-a-token", "elnat_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		recorder := s.do(http.MethodPost, nativeapi.RevokePath, url.Values{"token": {token}, "client_id": {testClientID}})
		if recorder.Code != http.StatusOK {
			t.Fatalf("revoke %q = %d", token, recorder.Code)
		}
	}
	if s.family(pair.DeviceID).revokedAt != nil {
		t.Fatal("revoking unknown tokens touched a live family")
	}
	unknownClient := s.do(http.MethodPost, nativeapi.RevokePath, url.Values{"token": {pair.RefreshToken}, "client_id": {"dev.elitea.nobody"}})
	if unknownClient.Code != http.StatusUnauthorized {
		t.Fatalf("revoke for an unknown client = %d, want 401 invalid_client", unknownClient.Code)
	}
}

func TestNativeRoutesAnswer404WithNoClientRegistered(t *testing.T) {
	pool := newPool(t)
	handler := nativeapi.New(nativeapi.Config{
		Registry: domain.NewRegistry(nil, pool), Store: domain.NewStore(pool, domain.Config{}), PublicOrigin: testOrigin,
	})
	router := newBareRouter(handler)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, nativeapi.AuthorizePath}, {http.MethodGet, nativeapi.ContinuePath},
		{http.MethodPost, nativeapi.DecisionPath}, {http.MethodPost, nativeapi.TokenPath},
		{http.MethodPost, nativeapi.RevokePath},
	} {
		if recorder := doOn(router, route.method, route.path, nil); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404 with no client registered", route.method, route.path, recorder.Code)
		}
	}
	if auth, err := handler.NativeAuth(context.Background(), testOrigin); err != nil || auth != nil {
		t.Fatalf("discovery native_auth = %+v (%v), want null", auth, err)
	}
}

func TestNativeLoopGuardStopsTheSignInBounce(t *testing.T) {
	never := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{}}`, http.StatusUnauthorized)
		})
	}
	s := newStack(t, withAuthenticate(never))
	start := s.do(http.MethodGet, authorizeQuery(nil), nil)
	binder := binderFrom(t, start)
	for bounce := 1; bounce <= 3; bounce++ {
		recorder := s.do(http.MethodGet, start.Header().Get("Location"), nil, withCookies(binder))
		if recorder.Code != http.StatusFound {
			t.Fatalf("bounce %d = %d", bounce, recorder.Code)
		}
	}
	stuck := s.do(http.MethodGet, start.Header().Get("Location"), nil, withCookies(binder))
	if stuck.Code != http.StatusBadRequest || !strings.Contains(stuck.Body.String(), "could not read the browser session") {
		t.Fatalf("fourth bounce = %d %s", stuck.Code, stuck.Body.String())
	}
}

func TestNativeContinueRefusesBearerAndForeignBrowser(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("bearer@example.test")
	pair := s.exchange(s.signIn(userID, "bearer@example.test"))
	start := s.do(http.MethodGet, authorizeQuery(nil), nil)
	binder := binderFrom(t, start)
	// A native access token must never approve a NEW device.
	withBearer := s.do(http.MethodGet, start.Header().Get("Location"), nil,
		withCookies(binder), withHeader("Authorization", "Bearer "+pair.AccessToken))
	if withBearer.Code != http.StatusBadRequest || strings.Contains(withBearer.Body.String(), "native-consent-form") {
		t.Fatalf("continue with a bearer = %d", withBearer.Code)
	}
	// Another browser (no binder cookie) gets a page and nothing else.
	foreign := s.do(http.MethodGet, start.Header().Get("Location"), nil,
		withCookies(sessionCookie(userID, "bearer@example.test")))
	if foreign.Code != http.StatusBadRequest || !strings.Contains(foreign.Body.String(), "different browser") {
		t.Fatalf("continue without the binder = %d %s", foreign.Code, foreign.Body.String())
	}
}

func TestNativeDecisionChecksOriginAndAccount(t *testing.T) {
	s := newStack(t)
	alice := s.seedUser("alice@example.test")
	bob := s.seedUser("bob@example.test")
	start := s.do(http.MethodGet, authorizeQuery(nil), nil)
	binder := binderFrom(t, start)
	consent := s.do(http.MethodGet, start.Header().Get("Location"), nil, withCookies(binder, sessionCookie(alice, "alice@example.test")))
	request := hiddenRequest.FindStringSubmatch(consent.Body.String())[1]
	uid := hiddenUID.FindStringSubmatch(consent.Body.String())[1]
	form := url.Values{"request": {request}, "uid": {uid}, "decision": {"allow"}}

	crossSite := s.do(http.MethodPost, nativeapi.DecisionPath, form,
		withCookies(binder, sessionCookie(alice, "alice@example.test")), withHeader("Origin", "https://evil.example"))
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-origin decision = %d", crossSite.Code)
	}
	// An opaque ("null") origin passes only with Sec-Fetch-Site: same-origin
	// (WebKit's consent POST); from anywhere else it is still refused.
	nullCrossSite := s.do(http.MethodPost, nativeapi.DecisionPath, form,
		withCookies(binder, sessionCookie(alice, "alice@example.test")),
		withHeader("Origin", "null"), withHeader("Sec-Fetch-Site", "cross-site"))
	if nullCrossSite.Code != http.StatusForbidden {
		t.Fatalf("null-origin cross-site decision = %d", nullCrossSite.Code)
	}
	swapped := s.do(http.MethodPost, nativeapi.DecisionPath, form,
		withCookies(binder, sessionCookie(bob, "bob@example.test")), withHeader("Origin", testOrigin))
	if swapped.Code != http.StatusConflict {
		t.Fatalf("decision after an account swap = %d", swapped.Code)
	}
	denied := s.do(http.MethodPost, nativeapi.DecisionPath,
		url.Values{"request": {request}, "uid": {uid}, "decision": {"deny"}},
		withCookies(binder, sessionCookie(alice, "alice@example.test")))
	location, _ := url.Parse(denied.Header().Get("Location"))
	if denied.Code != http.StatusFound || location.Query().Get("error") != "access_denied" || location.Query().Get("code") != "" {
		t.Fatalf("deny = %d %q", denied.Code, denied.Header().Get("Location"))
	}
	again := s.do(http.MethodPost, nativeapi.DecisionPath, form,
		withCookies(binder, sessionCookie(alice, "alice@example.test")))
	if again.Code != http.StatusBadRequest {
		t.Fatalf("second decision = %d, want a page (already answered)", again.Code)
	}
}

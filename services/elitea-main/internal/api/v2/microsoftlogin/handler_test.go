package microsoftlogin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

type fakeFlow struct {
	startErr  error
	started   microsoftlogin.StartRequest
	outcome   microsoftlogin.Outcome
	callback  microsoftlogin.CallbackParams
	redeemed  microsoftlogin.Redeemed
	redeemErr error
	redeem    microsoftlogin.RedeemRequest
}

func (f *fakeFlow) Start(_ context.Context, r microsoftlogin.StartRequest) (microsoftlogin.StartResult, error) {
	f.started = r
	if f.startErr != nil {
		return microsoftlogin.StartResult{}, f.startErr
	}
	return microsoftlogin.StartResult{
		AuthorizationURL: "https://login.microsoftonline.com/t/oauth2/v2.0/authorize?state=s", State: "s",
		ExpiresAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), App: "custom",
	}, nil
}

func (f *fakeFlow) Callback(_ context.Context, p microsoftlogin.CallbackParams) microsoftlogin.Outcome {
	f.callback = p
	return f.outcome
}

func (f *fakeFlow) Redeem(_ context.Context, r microsoftlogin.RedeemRequest) (microsoftlogin.Redeemed, error) {
	f.redeem = r
	return f.redeemed, f.redeemErr
}

func serve(h *Handler, method, path, body string, signedIn bool) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.With(DecodeStart).Post(StartPath, h.Start)
	r.With(DecodeRedeem).Post(RedeemPath, h.Redeem)
	r.Get(CallbackPath, h.Callback)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if signedIn {
		request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "11", UserID: "11"}))
	}
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)
	return recorder
}

func handler(flow Flow) *Handler {
	return New(flow, "https://elitea.example.com/", slog.New(slog.DiscardHandler))
}

func TestStartDecodesTheBodyAndAnswersTheAuthorizationURL(t *testing.T) {
	flow := &fakeFlow{}
	rec := serve(handler(flow), http.MethodPost, StartPath,
		`{"project_id":7,"credential_id":"c1","client_kind":"desktop","client_challenge":"ch","loopback_port":50000}`, true)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, microsoftlogin.StartRequest{
		UserID: 11, ProjectID: 7, CredentialID: "c1", ClientKind: "desktop", ClientChallenge: "ch", LoopbackPort: 50000,
	}, flow.started)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var view map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Equal(t, "s", view["state"])
	require.Equal(t, "custom", view["app"])
	require.EqualValues(t, 60, view["handoff_ttl_seconds"])
	require.Equal(t, ChannelName, view["channel_name"])
	require.Contains(t, view["authorization_url"], "login.microsoftonline.com")
}

func TestStartMapsRefusalsToStatuses(t *testing.T) {
	for code, want := range map[string]int{
		microsoftlogin.CodeCredentialNotFound:  http.StatusNotFound,
		microsoftlogin.CodeTooManySignIns:      http.StatusTooManyRequests,
		microsoftlogin.CodeClientKindInvalid:   http.StatusBadRequest,
		microsoftlogin.CodeClientChallenge:     http.StatusBadRequest,
		microsoftlogin.CodeCredentialIDInvalid: http.StatusBadRequest,
		microsoftlogin.CodeCustomAppsDisabled:  http.StatusConflict,
		"app_disabled":                         http.StatusConflict,
		microsoftlogin.CodeAuthorityInvalid:    http.StatusConflict,
	} {
		flow := &fakeFlow{startErr: &microsoftlogin.Refusal{Code: code, Message: "m"}}
		rec := serve(handler(flow), http.MethodPost, StartPath, `{"project_id":7,"credential_id":"c","client_kind":"web"}`, true)
		require.Equal(t, want, rec.Code, code)
		require.Contains(t, rec.Body.String(), `"code":"`+code+`"`)
	}
	rec := serve(handler(&fakeFlow{startErr: errors.New("db down: password=hunter2")}), http.MethodPost, StartPath,
		`{"project_id":7,"credential_id":"c","client_kind":"web"}`, true)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestStartAndRedeemRefuseBadBodiesAndAnonymousCallers(t *testing.T) {
	h := handler(&fakeFlow{})
	for _, path := range []string{StartPath, RedeemPath} {
		for _, body := range []string{``, `[]`, `{"project_id":0}`, `{"project_id":7,"extra":1}`, `{"project_id":"7"}`} {
			require.Equal(t, http.StatusBadRequest, serve(h, http.MethodPost, path, body, true).Code, "%s %q", path, body)
		}
	}
	require.Equal(t, http.StatusUnauthorized, serve(h, http.MethodPost, StartPath, `{"project_id":7}`, false).Code)
	require.Equal(t, http.StatusUnauthorized, serve(h, http.MethodPost, RedeemPath, `{"project_id":7}`, false).Code)
	big := `{"project_id":7,"handoff":"` + strings.Repeat("a", maxBody) + `"}`
	require.Equal(t, http.StatusBadRequest, serve(h, http.MethodPost, RedeemPath, big, true).Code)
}

func TestWithoutAFlowEveryRouteAnswers503(t *testing.T) {
	h := handler(nil)
	require.Equal(t, http.StatusServiceUnavailable, serve(h, http.MethodPost, StartPath, `{"project_id":7}`, true).Code)
	require.Equal(t, http.StatusServiceUnavailable, serve(h, http.MethodPost, RedeemPath, `{"project_id":7}`, true).Code)
	rec := serve(h, http.MethodGet, CallbackPath+"?state=s&code=c", "", false)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func TestRedeemAnswersTheTokenAndEveryRefusalIsTheSame(t *testing.T) {
	flow := &fakeFlow{redeemed: microsoftlogin.Redeemed{
		AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour), Scope: []string{"Mail.Read"},
		CredentialID: "c1", CredentialType: "outlook", App: "custom", TenantID: "t", AccountName: "A",
	}}
	rec := serve(handler(flow), http.MethodPost, RedeemPath,
		`{"project_id":7,"handoff":"h","state":"s","client_verifier":"v"}`, true)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, microsoftlogin.RedeemRequest{UserID: 11, ProjectID: 7, Handoff: "h", State: "s", ClientVerifier: "v"}, flow.redeem)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", rec.Header().Get("Pragma"))
	var view map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Equal(t, "at", view["access_token"])
	require.Equal(t, "Bearer", view["token_type"])
	// The browser receives an access token and its expiry: never a refresh token.
	for key := range view {
		require.NotContains(t, key, "refresh")
		require.NotContains(t, key, "secret")
	}
	require.Greater(t, view["expires_in"].(float64), float64(3500))

	refused := serve(handler(&fakeFlow{redeemErr: microsoftlogin.ErrHandoffInvalid}), http.MethodPost, RedeemPath,
		`{"project_id":7,"handoff":"h","state":"s"}`, true)
	require.Equal(t, http.StatusBadRequest, refused.Code)
	require.Contains(t, refused.Body.String(), `"code":"handoff_invalid"`)
	require.NotContains(t, refused.Body.String(), "at")
}

var scriptTag = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
var dataTag = regexp.MustCompile(`(?s)<script type="application/json" id="d">(.*?)</script>`)

func TestTheWebCallbackPageHandsTheCodeToTheOpenerUnderAStrictPolicy(t *testing.T) {
	flow := &fakeFlow{outcome: microsoftlogin.Outcome{Known: true, State: "st", ClientKind: microsoftlogin.ClientWeb, Handoff: "ho"}}
	rec := serve(handler(flow), http.MethodGet, CallbackPath+"?state=st&code=the-code&error=", "", false)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, microsoftlogin.CallbackParams{State: "st", Code: "the-code"}, flow.callback)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))

	body := rec.Body.String()
	data := dataTag.FindStringSubmatch(body)
	require.NotNil(t, data)
	var payload map[string]string
	require.NoError(t, json.Unmarshal([]byte(data[1]), &payload))
	require.Equal(t, map[string]string{
		"type": MessageType, "state": "st", "handoff": "ho", "origin": "https://elitea.example.com", "channel": ChannelName,
	}, payload)

	// The policy allows exactly the page's own script and style, by hash, and nothing
	// else: the data block never executes, and no other source is allowed.
	script := scriptTag.FindStringSubmatch(body)
	require.NotNil(t, script)
	sum := sha256.Sum256([]byte(script[1]))
	csp := rec.Header().Get("Content-Security-Policy")
	require.Contains(t, csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	require.Contains(t, csp, "default-src 'none'")
	require.Contains(t, csp, "frame-ancestors 'none'")
	require.NotContains(t, csp, "unsafe-inline")
	require.NotContains(t, csp, "unsafe-eval")
	// The page never carries the authorization code, a token or a secret.
	require.NotContains(t, body, "the-code")
}

func TestTheWebCallbackPageReportsAFailureByItsCodeWord(t *testing.T) {
	flow := &fakeFlow{outcome: microsoftlogin.Outcome{
		Known: true, State: "st", ClientKind: microsoftlogin.ClientWeb, ErrorCode: microsoftlogin.ErrPermission,
	}}
	rec := serve(handler(flow), http.MethodGet, CallbackPath+"?state=st&error=access_denied&error_description=<script>alert(1)</script>", "", false)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `"error":"permission_missing"`)
	require.NotContains(t, body, `"handoff":`)
	// Nothing Microsoft sent is rendered.
	require.NotContains(t, body, "alert(1)")
	require.Contains(t, body, "lacks a permission")
}

func TestDesktopAndMobileAreRedirectedWithTheHandoffCode(t *testing.T) {
	desktop := &fakeFlow{outcome: microsoftlogin.Outcome{
		Known: true, State: "st", ClientKind: microsoftlogin.ClientDesktop, ReturnPort: 50000, Handoff: "ho",
	}}
	rec := serve(handler(desktop), http.MethodGet, CallbackPath+"?state=st&code=c", "", false)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "http://127.0.0.1:50000/oauth/microsoft?handoff=ho&state=st", rec.Header().Get("Location"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))

	mobile := &fakeFlow{outcome: microsoftlogin.Outcome{
		Known: true, State: "st", ClientKind: microsoftlogin.ClientMobile, ReturnScheme: "elitea", Handoff: "ho",
	}}
	rec = serve(handler(mobile), http.MethodGet, CallbackPath+"?state=st&code=c", "", false)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "elitea://oauth/microsoft?handoff=ho&state=st", rec.Header().Get("Location"))

	failed := &fakeFlow{outcome: microsoftlogin.Outcome{
		Known: true, State: "st", ClientKind: microsoftlogin.ClientMobile, ReturnScheme: "elitea", ErrorCode: microsoftlogin.ErrDenied,
	}}
	rec = serve(handler(failed), http.MethodGet, CallbackPath+"?state=st&error=access_denied", "", false)
	require.Equal(t, "elitea://oauth/microsoft?state=st&error=denied", rec.Header().Get("Location"))
}

func TestAnUnknownStateGetsAPlainRefusal(t *testing.T) {
	rec := serve(handler(&fakeFlow{}), http.MethodGet, CallbackPath+"?state=nope&code=c", "", false)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))
	require.Contains(t, rec.Body.String(), "not valid or has expired")
	require.NotContains(t, rec.Body.String(), "<script")
}

func TestTheCallbackNeedsNoSession(t *testing.T) {
	// The system browser of a desktop or mobile sign-in holds no session, and a
	// popup may belong to a victim: the callback authenticates nothing and attaches
	// nothing. A signed-in user is irrelevant to it.
	flow := &fakeFlow{outcome: microsoftlogin.Outcome{Known: true, State: "s", ClientKind: microsoftlogin.ClientWeb, Handoff: "h"}}
	require.Equal(t, http.StatusOK, serve(handler(flow), http.MethodGet, CallbackPath+"?state=s&code=c", "", false).Code)
	require.Equal(t, http.StatusOK, serve(handler(flow), http.MethodGet, CallbackPath+"?state=s&code=c", "", true).Code)
}

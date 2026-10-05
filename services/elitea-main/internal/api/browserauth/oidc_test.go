package browserauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/html"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

func TestOIDCBeginDefaultsToBoundedPostAfterAdmissionAndStateAllocation(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginPath+"?target_to=%2Fprojects%2F7%3Ftab%3Dartifacts",
		nil,
	)
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Location") != "" ||
		recorder.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("location=%q content-type=%q", recorder.Header().Get("Location"), recorder.Header().Get("Content-Type"))
	}
	action, method, parameters := parseOIDCAuthorizationForm(t, recorder.Body.String())
	if action != "https://issuer.example/authorize" || method != "post" ||
		len(parameters) != 8 {
		t.Fatalf("action=%q method=%q parameters=%v", action, method, parameters)
	}
	expectedParameters := map[string]string{
		"response_type":         "code",
		"client_id":             "elitea",
		"redirect_uri":          "https://elitea.example/auth/oidc/login_callback",
		"scope":                 "openid profile email",
		"state":                 dependencies.flow.beginResult.TransactionID,
		"nonce":                 dependencies.protocol.authorization.Correlation.Nonce,
		"code_challenge":        dependencies.protocol.authorizationRequest.CodeChallenge,
		"code_challenge_method": OIDCPKCEChallengeS256,
	}
	for name, want := range expectedParameters {
		if values := parameters[name]; len(values) != 1 || values[0] != want {
			t.Fatalf("parameter %s = %v, want %q", name, values, want)
		}
	}
	script := oidcAuthorizationScript(t, recorder.Body.String())
	scriptDigest := sha256.Sum256([]byte(script))
	if "sha256-"+base64.StdEncoding.EncodeToString(scriptDigest[:]) != oidcAutoSubmitScriptHash {
		t.Fatalf("script hash changed for %q", script)
	}
	if strings.Contains(recorder.Body.String(), dependencies.protocol.authorization.ProviderState.PKCEVerifier) ||
		script != "document.getElementById(\"oidc-authorization\").submit();" ||
		!strings.Contains(recorder.Body.String(), `enctype="application/x-www-form-urlencoded"`) ||
		!strings.Contains(recorder.Body.String(), `accept-charset="UTF-8"`) ||
		!strings.Contains(recorder.Body.String(), "<noscript>") ||
		strings.Contains(recorder.Body.String(), "target_to") ||
		strings.Contains(recorder.Body.String(), "/projects/7?tab=artifacts") {
		t.Fatalf("unsafe or incomplete form body = %q", recorder.Body.String())
	}
	if strings.Join(*dependencies.events, ",") != "client-key,admit,new-authorization,begin,authorization-request" {
		t.Fatalf("events = %v", *dependencies.events)
	}
	if dependencies.admitter.attempt != (BrowserAttempt{
		ClientKey: "client-7",
		Stage:     BrowserAttemptOIDCBegin,
	}) {
		t.Fatalf("attempt = %+v", dependencies.admitter.attempt)
	}
	if dependencies.flow.beginRequest.Provider != "oidc" ||
		dependencies.flow.beginRequest.ReturnTarget != "/projects/7?tab=artifacts" ||
		dependencies.flow.beginRequest.Correlation != dependencies.protocol.authorization.Correlation ||
		dependencies.flow.beginRequest.ProviderState != dependencies.protocol.authorization.ProviderState {
		t.Fatalf("begin request = %+v", dependencies.flow.beginRequest)
	}
	if dependencies.protocol.authorization.PKCEChallengeMethod != OIDCPKCEChallengeS256 {
		t.Fatalf("PKCE method = %q", dependencies.protocol.authorization.PKCEChallengeMethod)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != CookieValuePrefix+dependencies.flow.beginResult.SessionID {
		t.Fatalf("cookies = %+v", cookies)
	}
	requireOIDCPostSecurityHeaders(t, recorder.Header(), "https://issuer.example")
}

func TestOIDCBeginRevokesExistingSessionBeforeAllocatingReplacement(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	oldSessionID := canonicalSessionID(6)
	request := httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath, nil)
	request.AddCookie(sessionCookie(oldSessionID))
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || dependencies.flow.logoutCalls != 1 ||
		dependencies.flow.logoutID != oldSessionID {
		t.Fatalf("status=%d logout=%d id=%q", recorder.Code, dependencies.flow.logoutCalls, dependencies.flow.logoutID)
	}
	if events := strings.Join(*dependencies.events, ","); events != "client-key,admit,new-authorization,logout,begin,authorization-request" {
		t.Fatalf("events = %s", events)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != CookieValuePrefix+dependencies.flow.beginResult.SessionID {
		t.Fatalf("cookies = %+v", cookies)
	}
}

func TestOIDCBeginExplicitGETRedirectsWithEquivalentBoundParameters(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.protocol.authorizationRequest.Transport = OIDCAuthorizationGET
	dependencies.protocol.authorizationRequest.Endpoint += "?prompt=login"
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath, nil),
	)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	parameters := location.Query()
	if location.Scheme != "https" || location.Host != "issuer.example" ||
		parameters.Get("prompt") != "login" || parameters.Get("response_type") != "code" ||
		parameters.Get("client_id") != "elitea" ||
		parameters.Get("redirect_uri") != "https://elitea.example/auth/oidc/login_callback" ||
		parameters.Get("scope") != "openid profile email" ||
		parameters.Get("state") != dependencies.flow.beginResult.TransactionID ||
		parameters.Get("nonce") != dependencies.protocol.authorization.Correlation.Nonce ||
		parameters.Get("code_challenge") != dependencies.protocol.authorizationRequest.CodeChallenge ||
		parameters.Get("code_challenge_method") != OIDCPKCEChallengeS256 || len(parameters) != 9 {
		t.Fatalf("authorization redirect = %q", location.String())
	}
	if len(recorder.Result().Cookies()) != 1 {
		t.Fatalf("cookies = %+v", recorder.Result().Cookies())
	}
	requireSecurityHeaders(t, recorder.Header())
}

func TestOIDCPostFormEscapesProviderConfigurationAsData(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.protocol.authorizationRequest.Endpoint += "?tenant=%22%3E%3Cform%20action%3Dhttps%3A%2F%2Fattacker.example%3E"
	dependencies.protocol.authorizationRequest.ClientID = `"><img/src=x>`
	dependencies.protocol.authorizationRequest.RedirectURI = "https://elitea.example/callback?next=%22%3E%3Cscript%3E"
	dependencies.protocol.authorizationRequest.Scope = "openid </script><script>TEST_ONLY"
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath, nil),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	action, _, parameters := parseOIDCAuthorizationForm(t, recorder.Body.String())
	if action != dependencies.protocol.authorizationRequest.Endpoint ||
		parameters.Get("client_id") != dependencies.protocol.authorizationRequest.ClientID ||
		parameters.Get("scope") != dependencies.protocol.authorizationRequest.Scope ||
		strings.Contains(recorder.Body.String(), "<img") ||
		strings.Contains(recorder.Body.String(), "</script><script>") {
		t.Fatalf("unsafe rendered form = %q", recorder.Body.String())
	}
	if script := oidcAuthorizationScript(t, recorder.Body.String()); script != "document.getElementById(\"oidc-authorization\").submit();" {
		t.Fatalf("script = %q", script)
	}
	requireOIDCPostSecurityHeaders(t, recorder.Header(), "https://issuer.example")
}

func TestOIDCBeginRejectsMalformedProtocolOutputAndRevokesStartedSession(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OIDCAuthorizationRequest)
	}{
		{name: "unknown transport", mutate: func(request *OIDCAuthorizationRequest) { request.Transport = "put" }},
		{name: "insecure endpoint", mutate: func(request *OIDCAuthorizationRequest) { request.Endpoint = "http://issuer.example/authorize" }},
		{name: "reserved endpoint query", mutate: func(request *OIDCAuthorizationRequest) { request.Endpoint += "?state=configured" }},
		{name: "duplicate endpoint query", mutate: func(request *OIDCAuthorizationRequest) { request.Endpoint += "?prompt=login&prompt=consent" }},
		{name: "secret endpoint query", mutate: func(request *OIDCAuthorizationRequest) { request.Endpoint += "?client_secret=TEST_ONLY_SECRET" }},
		{name: "unsafe CSP host", mutate: func(request *OIDCAuthorizationRequest) { request.Endpoint = "https://issuer.exämple/authorize" }},
		{name: "wrong response type", mutate: func(request *OIDCAuthorizationRequest) { request.ResponseType = "id_token" }},
		{name: "wrong state", mutate: func(request *OIDCAuthorizationRequest) { request.State = canonicalSessionID(7) }},
		{name: "wrong nonce", mutate: func(request *OIDCAuthorizationRequest) { request.Nonce = canonicalSessionID(8) }},
		{name: "wrong challenge", mutate: func(request *OIDCAuthorizationRequest) { request.CodeChallenge = strings.Repeat("A", 43) }},
		{name: "control client", mutate: func(request *OIDCAuthorizationRequest) { request.ClientID = "TEST_ONLY\nCLIENT" }},
		{name: "oversized client", mutate: func(request *OIDCAuthorizationRequest) {
			request.ClientID = strings.Repeat("x", maxOIDCAuthorizationValue+1)
		}},
		{name: "scope without openid", mutate: func(request *OIDCAuthorizationRequest) { request.Scope = "profile email" }},
		{name: "insecure redirect", mutate: func(request *OIDCAuthorizationRequest) { request.RedirectURI = "http://elitea.example/callback" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, dependencies := newTestOIDCHandler(t)
			test.mutate(&dependencies.protocol.authorizationRequest)
			recorder := httptest.NewRecorder()

			mountOIDC(handler).ServeHTTP(
				recorder,
				httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath, nil),
			)

			if recorder.Code != http.StatusServiceUnavailable ||
				recorder.Body.String() != http.StatusText(http.StatusServiceUnavailable) ||
				strings.Contains(recorder.Body.String(), "TEST_ONLY") {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			if dependencies.protocol.authorizationRequestCalls != 1 || dependencies.flow.logoutCalls != 1 ||
				dependencies.flow.logoutID != dependencies.flow.beginResult.SessionID {
				t.Fatalf("request=%d logout=%d id=%q", dependencies.protocol.authorizationRequestCalls, dependencies.flow.logoutCalls, dependencies.flow.logoutID)
			}
			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
				t.Fatalf("cookies = %+v", cookies)
			}
		})
	}
}

func TestOIDCBeginBoundsQueryBeforeProtocolAndStateAllocation(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginPath+"?target_to=/"+strings.Repeat("x", maxOIDCBeginQueryBytes),
		nil,
	)
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestURITooLong || dependencies.protocol.newAuthorizationCalls != 0 ||
		dependencies.flow.beginCalls != 0 || dependencies.flow.logoutCalls != 0 {
		t.Fatalf("status=%d protocol=%d begin=%d logout=%d", recorder.Code, dependencies.protocol.newAuthorizationCalls, dependencies.flow.beginCalls, dependencies.flow.logoutCalls)
	}
}

func TestOIDCPostFormEnforcesAggregateRenderedBound(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	request, err := dependencies.protocol.AuthorizationRequest(
		dependencies.flow.beginResult.TransactionID,
		dependencies.protocol.authorization,
	)
	if err != nil {
		t.Fatal(err)
	}
	endpointPrefix := "https://issuer.example/"
	request.Endpoint = endpointPrefix + strings.Repeat("x", maxOIDCAuthorizationURL-len(endpointPrefix))
	request.ClientID = strings.Repeat(`"`, maxOIDCAuthorizationValue)
	redirectPrefix := "https://elitea.example/"
	request.RedirectURI = redirectPrefix + strings.Repeat(`"`, maxOIDCAuthorizationValue-len(redirectPrefix))
	scopeParts := []string{"openid"}
	for len(scopeParts) < 16 {
		scopeParts = append(scopeParts, strings.Repeat(`"`, 256))
	}
	request.Scope = strings.Join(scopeParts, " ")
	if !validOIDCAuthorizationRequest(
		request,
		dependencies.flow.beginResult.TransactionID,
		dependencies.protocol.authorization,
	) {
		t.Fatal("aggregate-bound fixture must pass field validation")
	}
	if _, _, err := handler.renderOIDCAuthorizationForm(request); err == nil {
		t.Fatal("oversized rendered authorization form was accepted")
	}
}

func TestOIDCBeginAdmissionFailureHasNoProtocolOrStateEffects(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.admitter.err = ErrAttemptLimited
	dependencies.admitter.retryAfter = 1500 * time.Millisecond
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath, nil),
	)

	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "2" {
		t.Fatalf("status = %d Retry-After = %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if dependencies.protocol.newAuthorizationCalls != 0 || dependencies.flow.beginCalls != 0 ||
		dependencies.flow.logoutCalls != 0 {
		t.Fatalf("protocol=%d begin=%d logout=%d", dependencies.protocol.newAuthorizationCalls, dependencies.flow.beginCalls, dependencies.flow.logoutCalls)
	}
	if strings.Join(*dependencies.events, ",") != "client-key,admit" {
		t.Fatalf("events = %v", *dependencies.events)
	}
}

func TestOIDCCallbackSupportsCurrentGETAndPOSTShapes(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			handler, dependencies := newTestOIDCHandler(t)
			state := dependencies.flow.beginResult.TransactionID
			values := url.Values{"state": {state}, "code": {"authorization-code"}}
			var request *http.Request
			if method == http.MethodGet {
				request = httptest.NewRequest(method, BasePath+OIDCLoginCallbackPath+"?"+values.Encode(), nil)
			} else {
				request = httptest.NewRequest(method, BasePath+OIDCLoginCallbackPath, strings.NewReader(values.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
			recorder := httptest.NewRecorder()

			mountOIDC(handler).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusFound ||
				recorder.Header().Get("Location") != dependencies.flow.completeResult.ReturnTarget {
				t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
			}
			if strings.Join(*dependencies.events, ",") != "client-key,admit,new-verifier,complete" {
				t.Fatalf("events = %v", *dependencies.events)
			}
			if dependencies.admitter.attempt.Stage != BrowserAttemptOIDCCallback ||
				dependencies.protocol.code != "authorization-code" {
				t.Fatalf("attempt = %+v code = %q", dependencies.admitter.attempt, dependencies.protocol.code)
			}
			if dependencies.flow.completeRequest != (browserapp.CompleteRequest{
				SessionID:     dependencies.flow.beginResult.SessionID,
				TransactionID: state,
				Provider:      "oidc",
			}) {
				t.Fatalf("complete request = %+v", dependencies.flow.completeRequest)
			}
			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 ||
				cookies[0].Value != CookieValuePrefix+dependencies.flow.completeResult.SessionID {
				t.Fatalf("rotated cookies = %+v", cookies)
			}
			requireSecurityHeaders(t, recorder.Header())
		})
	}
}

func TestOIDCCallbackRejectsAmbiguousInputsAfterAdmissionWithoutProviderWork(t *testing.T) {
	state := canonicalSessionID(2)
	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "duplicate state", method: http.MethodGet, target: "?state=" + state + "&state=" + state + "&code=c", wantStatus: http.StatusBadRequest},
		{name: "code and error", method: http.MethodGet, target: "?state=" + state + "&code=c&error=denied", wantStatus: http.StatusBadRequest},
		{name: "ignored issuer forbidden", method: http.MethodGet, target: "?state=" + state + "&code=c&iss=https%3A%2F%2Fother.example", wantStatus: http.StatusBadRequest},
		{name: "implicit id token forbidden", method: http.MethodGet, target: "?state=" + state + "&id_token=unsigned", wantStatus: http.StatusBadRequest},
		{name: "unknown parameter", method: http.MethodGet, target: "?state=" + state + "&code=c&token=secret", wantStatus: http.StatusBadRequest},
		{name: "malformed query", method: http.MethodGet, target: "?state=%zz&code=c", wantStatus: http.StatusBadRequest},
		{name: "success with error description", method: http.MethodGet, target: "?state=" + state + "&code=c&error_description=no", wantStatus: http.StatusBadRequest},
		{name: "post query ambiguity", method: http.MethodPost, target: "?state=" + state, body: "state=" + state + "&code=c", contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusBadRequest},
		{name: "post wrong media", method: http.MethodPost, body: `{"state":"` + state + `"}`, contentType: "application/json", wantStatus: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, dependencies := newTestOIDCHandler(t)
			request := httptest.NewRequest(test.method, BasePath+OIDCLoginCallbackPath+test.target, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
			recorder := httptest.NewRecorder()
			mountOIDC(handler).ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			if dependencies.admitter.calls != 1 ||
				dependencies.admitter.attempt.Stage != BrowserAttemptOIDCCallback ||
				dependencies.flow.completeCalls != 0 ||
				dependencies.protocol.newVerifierCalls != 0 {
				t.Fatalf("admit=%d complete=%d verifier=%d", dependencies.admitter.calls, dependencies.flow.completeCalls, dependencies.protocol.newVerifierCalls)
			}
		})
	}
}

func TestOIDCCallbackGatesOversizedAndBodySmugglingAttemptsBeforeParsing(t *testing.T) {
	t.Run("limited oversized body is not parsed", func(t *testing.T) {
		handler, dependencies := newTestOIDCHandler(t)
		dependencies.admitter.err = ErrAttemptLimited
		request := httptest.NewRequest(
			http.MethodPost,
			BasePath+OIDCLoginCallbackPath,
			strings.NewReader(strings.Repeat("x", int(DefaultMaxOIDCCallbackBytes+1))),
		)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusTooManyRequests || dependencies.admitter.calls != 1 ||
			dependencies.flow.completeCalls != 0 {
			t.Fatalf("status=%d admit=%d complete=%d", recorder.Code, dependencies.admitter.calls, dependencies.flow.completeCalls)
		}
	})

	t.Run("admitted oversized body is bounded", func(t *testing.T) {
		handler, dependencies := newTestOIDCHandler(t)
		request := httptest.NewRequest(
			http.MethodPost,
			BasePath+OIDCLoginCallbackPath,
			strings.NewReader(strings.Repeat("x", int(DefaultMaxOIDCCallbackBytes+1))),
		)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge || dependencies.admitter.calls != 1 ||
			dependencies.flow.completeCalls != 0 {
			t.Fatalf("status=%d admit=%d complete=%d", recorder.Code, dependencies.admitter.calls, dependencies.flow.completeCalls)
		}
	})

	t.Run("duplicate content type", func(t *testing.T) {
		handler, dependencies := newTestOIDCHandler(t)
		request := httptest.NewRequest(http.MethodPost, BasePath+OIDCLoginCallbackPath, strings.NewReader("state=x"))
		request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Add("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnsupportedMediaType || dependencies.admitter.calls != 1 {
			t.Fatalf("status=%d admit=%d", recorder.Code, dependencies.admitter.calls)
		}
	})

	t.Run("GET transfer encoding body", func(t *testing.T) {
		handler, dependencies := newTestOIDCHandler(t)
		request := httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginCallbackPath, nil)
		request.Body = io.NopCloser(strings.NewReader("state=hidden"))
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || dependencies.admitter.calls != 1 ||
			dependencies.flow.completeCalls != 0 {
			t.Fatalf("status=%d admit=%d complete=%d", recorder.Code, dependencies.admitter.calls, dependencies.flow.completeCalls)
		}
	})
}

func TestOIDCCallbackAdmissionPrecedesCookieAndProviderWork(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.admitter.err = ErrAttemptLimited
	state := dependencies.flow.beginResult.TransactionID
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginCallbackPath+"?state="+state+"&code=authorization-code",
		nil,
	)
	// Deliberately omit the cookie: a well-formed attempt is limited before
	// cookie decoding and before verifier/token/JWKS work.
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests || dependencies.admitter.calls != 1 ||
		dependencies.protocol.newVerifierCalls != 0 || dependencies.flow.completeCalls != 0 {
		t.Fatalf("status=%d admit=%d verifier=%d complete=%d", recorder.Code, dependencies.admitter.calls, dependencies.protocol.newVerifierCalls, dependencies.flow.completeCalls)
	}
	if strings.Join(*dependencies.events, ",") != "client-key,admit" {
		t.Fatalf("events = %v", *dependencies.events)
	}
}

func TestOIDCProviderErrorConsumesThroughTheOneTimeFlow(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.flow.completeErr = browserapp.ErrUnauthenticated
	state := dependencies.flow.beginResult.TransactionID
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginCallbackPath+"?state="+state+"&error=access_denied&error_description=cancelled",
		nil,
	)
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != BasePath+LoginPath+"?error=true" {
		t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if dependencies.protocol.newVerifierCalls != 0 || dependencies.flow.completeCalls != 1 {
		t.Fatalf("verifier=%d complete=%d", dependencies.protocol.newVerifierCalls, dependencies.flow.completeCalls)
	}
	assertion, err := dependencies.flow.completeVerifier.Verify(context.Background(), browserflow.VerificationContext{})
	if !errors.Is(err, errOIDCCallbackRejected) || assertion.Provider != "" {
		t.Fatalf("rejected verifier = %+v, %v", assertion, err)
	}
}

func TestOIDCCallbackMapsAuthenticationAndDependencyFailures(t *testing.T) {
	tests := []struct {
		name       string
		flowErr    error
		wantStatus int
		location   string
	}{
		{name: "invalid token", flowErr: browserapp.ErrUnauthenticated, wantStatus: http.StatusFound, location: BasePath + LoginPath + "?error=true"},
		{name: "provider outage", flowErr: browserapp.ErrDependencyUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "consumed state", flowErr: browserapp.ErrTransactionRejected, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, dependencies := newTestOIDCHandler(t)
			dependencies.flow.completeErr = test.flowErr
			request := callbackRequest(dependencies, "authorization-code")
			recorder := httptest.NewRecorder()
			mountOIDC(handler).ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || recorder.Header().Get("Location") != test.location {
				t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
			}
		})
	}
}

func TestOIDCCallbackRevokesInvalidRotatedSessionBeforeClearingCookie(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	dependencies.flow.completeResult.ReturnTarget = "https://attacker.example"
	recorder := httptest.NewRecorder()

	mountOIDC(handler).ServeHTTP(recorder, callbackRequest(dependencies, "authorization-code"))

	if recorder.Code != http.StatusServiceUnavailable ||
		dependencies.flow.logoutCalls != 1 ||
		dependencies.flow.logoutID != dependencies.flow.completeResult.SessionID {
		t.Fatalf("status=%d logout=%d id=%q", recorder.Code, dependencies.flow.logoutCalls, dependencies.flow.logoutID)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("cookies = %+v", cookies)
	}
}

func TestOIDCRoutesPreservePathsAndRejectMutatingHEADWithoutProductionMount(t *testing.T) {
	handler, dependencies := newTestOIDCHandler(t)
	for path, allow := range map[string]string{
		OIDCLoginPath:         "GET, OPTIONS",
		OIDCLoginCallbackPath: "GET, POST, OPTIONS",
	} {
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodOptions, BasePath+path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Allow") != allow {
			t.Fatalf("%s: status = %d Allow = %q", path, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
	for path, allow := range map[string]string{
		OIDCLoginPath:         "GET, OPTIONS",
		OIDCLoginCallbackPath: "GET, POST, OPTIONS",
	} {
		recorder := httptest.NewRecorder()
		mountOIDC(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, BasePath+path, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != allow ||
			recorder.Body.Len() != 0 {
			t.Fatalf("%s HEAD: status=%d Allow=%q body=%q", path, recorder.Code, recorder.Header().Get("Allow"), recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	mountOIDC(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, BasePath+OIDCLoginPath, nil))
	if recorder.Code != http.StatusBadRequest || dependencies.admitter.calls != 0 ||
		dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 {
		t.Fatalf("status=%d admit=%d begin=%d complete=%d", recorder.Code, dependencies.admitter.calls, dependencies.flow.beginCalls, dependencies.flow.completeCalls)
	}
}

func TestOIDCLifecycleAcrossRealHTTPAndApplicationBoundaries(t *testing.T) {
	sessions := &componentSessionStore{records: make(map[string]sessionstate.State)}
	transactions := &componentTransactionStore{records: make(map[string]browserflow.Transaction)}
	provisioner := &componentProvisioner{result: identity.ProvisionResult{UserID: 71}}
	flow, err := browserapp.NewService(
		sessions,
		transactions,
		provisioner,
		componentPrincipalValidator{},
		5*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]string, 0, 8)
	protocol := newOIDCProtocolStub(&events)
	protocol.verify = func(_ context.Context, verification browserflow.VerificationContext, code string) (browserflow.VerifiedAssertion, error) {
		if code != "authorization-code" {
			return browserflow.VerifiedAssertion{}, errors.New("invalid authorization code")
		}
		expiresAt := time.Now().Add(time.Hour).UTC()
		return browserflow.VerifiedAssertion{
			Provider:            "oidc",
			ProviderReference:   "admin",
			Email:               "admin@example.test",
			GivenName:           "Ada",
			FamilyName:          "Lovelace",
			Name:                "Ada Lovelace",
			ProviderAttributes:  json.RawMessage(`{"nameid":"admin","attributes":{"picture":"https://images.example/avatar.png"},"sessionindex":""}`),
			Expiration:          &expiresAt,
			ProtocolCorrelation: verification.Correlation,
		}, nil
	}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "centry_auth_session", Secure: true,
		SameSite: http.SameSiteLaxMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewOIDCHandler(
		flow,
		protocol,
		&attemptAdmitterStub{events: &events},
		&clientKeyResolverStub{events: &events, key: "client-7"},
		cookies,
		OIDCHandlerConfig{DefaultLoginTarget: "/"},
	)
	if err != nil {
		t.Fatal(err)
	}

	beginRecorder := httptest.NewRecorder()
	mountOIDC(handler).ServeHTTP(
		beginRecorder,
		httptest.NewRequest(http.MethodGet, BasePath+OIDCLoginPath+"?target_to=%2Fafter", nil),
	)
	if beginRecorder.Code != http.StatusOK {
		t.Fatalf("begin status = %d", beginRecorder.Code)
	}
	beginCookies := beginRecorder.Result().Cookies()
	_, _, beginParameters := parseOIDCAuthorizationForm(t, beginRecorder.Body.String())
	if len(beginCookies) != 1 {
		t.Fatalf("begin cookies=%d", len(beginCookies))
	}
	transactionID := beginParameters.Get("state")
	if browserflow.ValidateTransactionID(transactionID) != nil {
		t.Fatalf("transaction ID = %q", transactionID)
	}
	callback := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginCallbackPath+"?state="+transactionID+"&code=authorization-code",
		nil,
	)
	callback.AddCookie(beginCookies[0])
	callbackRecorder := httptest.NewRecorder()
	mountOIDC(handler).ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusFound || callbackRecorder.Header().Get("Location") != "/after" {
		t.Fatalf("callback status=%d location=%q body=%q", callbackRecorder.Code, callbackRecorder.Header().Get("Location"), callbackRecorder.Body.String())
	}
	if provisioner.request.Assertion.Provider != "oidc" ||
		provisioner.request.Assertion.ProviderReference != "admin" ||
		provisioner.request.Assertion.Email != "admin@example.test" {
		t.Fatalf("provision request = %+v", provisioner.request)
	}
	if protocol.verification.Correlation != protocol.authorization.Correlation ||
		protocol.verification.ProviderState != protocol.authorization.ProviderState {
		t.Fatalf("verification = %+v", protocol.verification)
	}
	authenticatedCookies := callbackRecorder.Result().Cookies()
	if len(authenticatedCookies) != 1 || authenticatedCookies[0].Value == beginCookies[0].Value {
		t.Fatalf("authenticated cookies = %+v", authenticatedCookies)
	}

	// The callback transaction is one-time even when the browser replays the
	// exact bounded response.
	replay := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginCallbackPath+"?state="+transactionID+"&code=authorization-code",
		nil,
	)
	replay.AddCookie(beginCookies[0])
	replayRecorder := httptest.NewRecorder()
	mountOIDC(handler).ServeHTTP(replayRecorder, replay)
	if replayRecorder.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d", replayRecorder.Code)
	}
}

func TestNewOIDCHandlerRejectsIncompleteOrUnsafeConfiguration(t *testing.T) {
	_, dependencies := newTestOIDCHandler(t)
	strictCookies, err := NewCookiePolicy(CookieConfig{
		Name: "strict_session", Secure: true,
		SameSite: http.SameSiteStrictMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := OIDCHandlerConfig{DefaultLoginTarget: "/", MaxCallbackBytes: 4096}
	tests := []struct {
		name       string
		flow       Flow
		protocol   OIDCProtocol
		attempts   AttemptAdmitter
		clientKeys ClientKeyResolver
		cookies    *CookiePolicy
		config     OIDCHandlerConfig
	}{
		{name: "missing flow", protocol: dependencies.protocol, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing protocol", flow: dependencies.flow, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing admission", flow: dependencies.flow, protocol: dependencies.protocol, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing client policy", flow: dependencies.flow, protocol: dependencies.protocol, attempts: dependencies.admitter, cookies: dependencies.cookies, config: valid},
		{name: "missing cookie policy", flow: dependencies.flow, protocol: dependencies.protocol, attempts: dependencies.admitter, clientKeys: dependencies.resolver, config: valid},
		{name: "strict callback cookie", flow: dependencies.flow, protocol: dependencies.protocol, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: strictCookies, config: valid},
		{name: "external target", flow: dependencies.flow, protocol: dependencies.protocol, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: OIDCHandlerConfig{DefaultLoginTarget: "https://attacker.example", MaxCallbackBytes: 4096}},
		{name: "unbounded callback", flow: dependencies.flow, protocol: dependencies.protocol, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: OIDCHandlerConfig{DefaultLoginTarget: "/", MaxCallbackBytes: maxMaxOIDCCallbackBytes + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewOIDCHandler(test.flow, test.protocol, test.attempts, test.clientKeys, test.cookies, test.config)
			if !errors.Is(err, ErrInvalidHandlerConfiguration) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type oidcTestDependencies struct {
	flow     *flowStub
	protocol *oidcProtocolStub
	admitter *attemptAdmitterStub
	resolver *clientKeyResolverStub
	cookies  *CookiePolicy
	events   *[]string
}

func FuzzOIDCAuthorizationEndpointValidation(f *testing.F) {
	for _, seed := range []string{
		"https://issuer.example/authorize",
		"https://issuer.example/authorize?prompt=login",
		"http://issuer.example/authorize",
		"https://issuer.example/authorize?state=configured",
		"https://issuer.example/authorize?client_secret=TEST_ONLY_SECRET",
		"https://issuer.example/authorize?prompt=%zz",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		parsed, origin, ok := parseOIDCAuthorizationEndpoint(value)
		if !ok {
			return
		}
		if parsed.Scheme != "https" || parsed.Host == "" || origin != "https://"+parsed.Host ||
			strings.ContainsAny(origin, " \t\r\n;,'\"") {
			t.Fatalf("unsafe accepted endpoint=%q origin=%q", parsed.String(), origin)
		}
		parameters, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		for key, values := range parameters {
			if isOIDCAuthorizationParameter(key) || forbiddenOIDCAuthorizationEndpointParameter(key) ||
				len(values) != 1 {
				t.Fatalf("ambiguous accepted query = %v", parameters)
			}
		}
	})
}

func newTestOIDCHandler(t *testing.T) (*OIDCHandler, *oidcTestDependencies) {
	t.Helper()
	events := make([]string, 0, 8)
	flow := &flowStub{
		events: &events,
		beginResult: browserapp.BeginResult{
			SessionID:     canonicalSessionID(1),
			TransactionID: canonicalSessionID(2),
			ExpiresAt:     time.Now().Add(5 * time.Minute),
		},
		completeResult: browserapp.CompleteResult{
			SessionID:    canonicalSessionID(3),
			ReturnTarget: "/after-login?auth_state=popup-7",
		},
	}
	protocol := newOIDCProtocolStub(&events)
	admitter := &attemptAdmitterStub{events: &events}
	resolver := &clientKeyResolverStub{events: &events, key: "client-7"}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "centry_auth_session", Secure: true,
		SameSite: http.SameSiteLaxMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewOIDCHandler(flow, protocol, admitter, resolver, cookies, OIDCHandlerConfig{
		DefaultLoginTarget: "/safe-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, &oidcTestDependencies{
		flow: flow, protocol: protocol, admitter: admitter, resolver: resolver,
		cookies: cookies, events: &events,
	}
}

func mountOIDC(handler *OIDCHandler) http.Handler {
	router := chi.NewRouter()
	router.Mount(BasePath, handler.Routes())
	return router
}

func parseOIDCAuthorizationForm(t *testing.T, body string) (string, string, url.Values) {
	t.Helper()
	document, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var form *html.Node
	var findForm func(*html.Node)
	findForm = func(node *html.Node) {
		if form != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == "form" {
			form = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			findForm(child)
		}
	}
	findForm(document)
	if form == nil {
		t.Fatal("OIDC authorization form is absent")
	}
	action := htmlAttribute(form, "action")
	method := htmlAttribute(form, "method")
	parameters := make(url.Values)
	var collectInputs func(*html.Node)
	collectInputs = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "input" && htmlAttribute(node, "type") == "hidden" {
			parameters.Add(htmlAttribute(node, "name"), htmlAttribute(node, "value"))
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collectInputs(child)
		}
	}
	collectInputs(form)
	return action, method, parameters
}

func htmlAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

func oidcAuthorizationScript(t *testing.T, body string) string {
	t.Helper()
	document, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	scripts := make([]string, 0, 1)
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "script" {
			text := ""
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.TextNode {
					text += child.Data
				}
			}
			scripts = append(scripts, text)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	if len(scripts) != 1 {
		t.Fatalf("scripts = %v", scripts)
	}
	return scripts[0]
}

func requireOIDCPostSecurityHeaders(t *testing.T, headers http.Header, origin string) {
	t.Helper()
	for name, expected := range map[string]string{
		"Cache-Control":          "no-store",
		"Pragma":                 "no-cache",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Server":                 "Centry",
	} {
		if got := headers.Get(name); got != expected {
			t.Fatalf("%s = %q, want %q", name, got, expected)
		}
	}
	csp := headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "form-action "+origin) ||
		!strings.Contains(csp, "script-src '"+oidcAutoSubmitScriptHash+"'") ||
		strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP = %q", csp)
	}
}

func callbackRequest(dependencies *oidcTestDependencies, code string) *http.Request {
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+OIDCLoginCallbackPath+"?"+url.Values{
			"state": {dependencies.flow.beginResult.TransactionID},
			"code":  {code},
		}.Encode(),
		nil,
	)
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	return request
}

type oidcProtocolStub struct {
	events                    *[]string
	authorization             OIDCAuthorization
	authorizationRequest      OIDCAuthorizationRequest
	authorizationErr          error
	authorizationRequestErr   error
	code                      string
	verification              browserflow.VerificationContext
	verify                    func(context.Context, browserflow.VerificationContext, string) (browserflow.VerifiedAssertion, error)
	newAuthorizationCalls     int
	authorizationRequestCalls int
	newVerifierCalls          int
}

func newOIDCProtocolStub(events *[]string) *oidcProtocolStub {
	stub := &oidcProtocolStub{
		events: events,
		authorization: OIDCAuthorization{
			Correlation:         browserflow.ProtocolCorrelation{Nonce: canonicalSessionID(9)},
			ProviderState:       browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
			PKCEChallengeMethod: OIDCPKCEChallengeS256,
		},
		authorizationRequest: OIDCAuthorizationRequest{
			Transport:           OIDCAuthorizationPOST,
			Endpoint:            "https://issuer.example/authorize",
			ResponseType:        "code",
			ClientID:            "elitea",
			RedirectURI:         "https://elitea.example/auth/oidc/login_callback",
			Scope:               "openid profile email",
			CodeChallengeMethod: OIDCPKCEChallengeS256,
		},
	}
	challenge := sha256.Sum256([]byte(stub.authorization.ProviderState.PKCEVerifier))
	stub.authorizationRequest.CodeChallenge = base64.RawURLEncoding.EncodeToString(challenge[:])
	return stub
}

func (stub *oidcProtocolStub) NewAuthorization(context.Context) (OIDCAuthorization, error) {
	stub.newAuthorizationCalls++
	*stub.events = append(*stub.events, "new-authorization")
	return stub.authorization, stub.authorizationErr
}

func (stub *oidcProtocolStub) AuthorizationRequest(
	state string,
	authorization OIDCAuthorization,
) (OIDCAuthorizationRequest, error) {
	stub.authorizationRequestCalls++
	*stub.events = append(*stub.events, "authorization-request")
	result := stub.authorizationRequest
	if result.State == "" {
		result.State = state
	}
	if result.Nonce == "" {
		result.Nonce = authorization.Correlation.Nonce
	}
	if result.CodeChallenge == "" {
		challenge := sha256.Sum256([]byte(authorization.ProviderState.PKCEVerifier))
		result.CodeChallenge = base64.RawURLEncoding.EncodeToString(challenge[:])
	}
	return result, stub.authorizationRequestErr
}

func (stub *oidcProtocolStub) NewVerifier(code string) browserapp.AssertionVerifier {
	stub.newVerifierCalls++
	stub.code = code
	*stub.events = append(*stub.events, "new-verifier")
	return &oidcVerifierStub{protocol: stub, code: code}
}

type oidcVerifierStub struct {
	protocol *oidcProtocolStub
	code     string
}

func (stub *oidcVerifierStub) Verify(
	ctx context.Context,
	verification browserflow.VerificationContext,
) (browserflow.VerifiedAssertion, error) {
	stub.protocol.verification = verification
	if stub.protocol.verify == nil {
		return browserflow.VerifiedAssertion{}, fmt.Errorf("test verifier has no implementation")
	}
	return stub.protocol.verify(ctx, verification, stub.code)
}

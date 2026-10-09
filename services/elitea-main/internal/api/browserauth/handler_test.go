package browserauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

func TestRoutesPreserveExactFormSurfaceAndMethods(t *testing.T) {
	handler, _ := newTestHandler(t)
	observed := make(map[string][]string)
	if err := chi.Walk(handler.Routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		observed[route] = append(observed[route], method)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for path := range observed {
		sort.Strings(observed[path])
	}
	expected := map[string][]string{
		LoginPath:         {http.MethodGet, http.MethodHead, http.MethodOptions},
		LogoutPath:        {http.MethodGet, http.MethodHead, http.MethodOptions},
		FormLoginPath:     {http.MethodGet, http.MethodHead, http.MethodOptions},
		FormAuthorizePath: {http.MethodOptions, http.MethodPost},
		FormLogoutPath:    {http.MethodGet, http.MethodHead, http.MethodOptions},
	}
	if len(observed) != len(expected) {
		t.Fatalf("routes = %v, want %v", observed, expected)
	}
	for path, methods := range expected {
		if strings.Join(observed[path], ",") != strings.Join(methods, ",") {
			t.Fatalf("methods for %s = %v, want %v", path, observed[path], methods)
		}
	}
}

func TestBeginLoginCreatesBoundTransactionAndVersionedCookie(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	target := "/elitea_ui/auth-callback?auth_state=popup-7"
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+LoginPath+"?target_to="+url.QueryEscape(target),
		nil,
	)
	recorder := httptest.NewRecorder()

	mount(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusFound)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != BasePath+FormLoginPath ||
		location.Query().Get("target_to") != dependencies.flow.beginResult.TransactionID {
		t.Fatalf("location = %q", location.String())
	}
	if dependencies.flow.beginRequest.Provider != "form" ||
		dependencies.flow.beginRequest.ReturnTarget != target {
		t.Fatalf("begin request = %+v", dependencies.flow.beginRequest)
	}
	if dependencies.admitter.attempt.Stage != BrowserAttemptFormBegin ||
		dependencies.admitter.attempt.ClientKey != "client-7" ||
		dependencies.admitter.attempt.LoginDigest != ([sha256.Size]byte{}) {
		t.Fatalf("begin attempt = %+v", dependencies.admitter.attempt)
	}
	if strings.Join(*dependencies.events, ",") != "client-key,admit,begin" {
		t.Fatalf("events = %v", *dependencies.events)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != CookieValuePrefix+dependencies.flow.beginResult.SessionID {
		t.Fatalf("cookies = %+v", cookies)
	}
	requireSecurityHeaders(t, recorder.Header())
}

func TestBeginLoginAdmitsBeforeWritesAndRevokesExistingSession(t *testing.T) {
	t.Run("limited before state write", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.admitter.err = ErrAttemptLimited
		dependencies.admitter.retryAfter = 2 * time.Second
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, BasePath+LoginPath, nil))
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "2" ||
			dependencies.flow.beginCalls != 0 || dependencies.flow.logoutCalls != 0 {
			t.Fatalf("status = %d begin = %d logout = %d", recorder.Code, dependencies.flow.beginCalls, dependencies.flow.logoutCalls)
		}
		if strings.Join(*dependencies.events, ",") != "client-key,admit" {
			t.Fatalf("events = %v", *dependencies.events)
		}
	})

	t.Run("admission outage before state write", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.admitter.err = errors.New("limiter unavailable for secret-canary")
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, BasePath+LoginPath, nil))
		if recorder.Code != http.StatusServiceUnavailable || dependencies.flow.beginCalls != 0 ||
			dependencies.flow.logoutCalls != 0 || strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("status = %d begin = %d logout = %d body = %q", recorder.Code, dependencies.flow.beginCalls, dependencies.flow.logoutCalls, recorder.Body.String())
		}
	})

	t.Run("existing session consumed before replacement", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		oldSessionID := canonicalSessionID(9)
		request := httptest.NewRequest(http.MethodGet, BasePath+LoginPath, nil)
		request.AddCookie(sessionCookie(oldSessionID))
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusFound || dependencies.flow.logoutID != oldSessionID ||
			dependencies.flow.logoutCalls != 1 || dependencies.flow.beginCalls != 1 {
			t.Fatalf("status = %d logout ID = %q logout = %d begin = %d", recorder.Code, dependencies.flow.logoutID, dependencies.flow.logoutCalls, dependencies.flow.beginCalls)
		}
		if strings.Join(*dependencies.events, ",") != "client-key,admit,logout,begin" {
			t.Fatalf("events = %v", *dependencies.events)
		}
	})

	t.Run("uncertain revocation fails closed", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.logoutErr = errors.New("store unavailable for secret-canary")
		request := httptest.NewRequest(http.MethodGet, BasePath+LoginPath, nil)
		request.AddCookie(sessionCookie(canonicalSessionID(9)))
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || dependencies.flow.beginCalls != 0 ||
			strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("status = %d begin = %d body = %q", recorder.Code, dependencies.flow.beginCalls, recorder.Body.String())
		}
		if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
			t.Fatalf("cookies = %+v, want deletion", cookies)
		}
	})
}

func TestSetRetryAfterPreservesSupportedWindowsAndBoundsDependencies(t *testing.T) {
	for _, test := range []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{name: "rounds up", duration: 90*time.Minute + time.Millisecond, want: "5401"},
		{name: "preserves maximum", duration: browserapp.MaxBrowserAttemptRetryAfter, want: "86400"},
		{name: "bounds invalid dependency result", duration: 48 * time.Hour, want: "86400"},
		{name: "omits non-positive", duration: 0, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			setRetryAfter(recorder, test.duration)
			if got := recorder.Header().Get("Retry-After"); got != test.want {
				t.Fatalf("Retry-After = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBeginLoginFallsBackFromUntrustedOrAmbiguousTarget(t *testing.T) {
	for _, rawQuery := range []string{
		"",
		"target_to=https%3A%2F%2Fattacker.example",
		"target_to=%2F%2Fattacker.example",
		"target_to=%2Fok&target_to=%2Fother",
		"target_to=%2F%5Cattacker.example",
		"target_to=%2Fok%0D%0ALocation%3Aevil",
		"target_to=%2F%252Fattacker.example",
		"target_to=%2F%255Cattacker.example",
		"target_to=%2Fok%250D%250ALocation%3Aevil",
	} {
		t.Run(rawQuery, func(t *testing.T) {
			handler, dependencies := newTestHandler(t)
			path := BasePath + LoginPath
			if rawQuery != "" {
				path += "?" + rawQuery
			}
			recorder := httptest.NewRecorder()
			mount(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusFound {
				t.Fatalf("status = %d", recorder.Code)
			}
			if dependencies.flow.beginRequest.ReturnTarget != "/safe-default" {
				t.Fatalf("target = %q", dependencies.flow.beginRequest.ReturnTarget)
			}
		})
	}
}

func FuzzQueryReturnTargetNeverReturnsAnUnsafeRedirect(f *testing.F) {
	for _, seed := range []string{
		"/",
		"/elitea_ui/auth-callback?auth_state=popup-7",
		"https://attacker.example",
		"//attacker.example",
		"/\\attacker.example",
		"/ok\r\nLocation: evil",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, candidate string) {
		got := queryReturnTarget(url.Values{"target_to": {candidate}}, "/safe-default")
		if browserflow.ValidateReturnTarget(got) != nil {
			t.Fatalf("unsafe target escaped validation: %q", got)
		}
		if browserflow.ValidateReturnTarget(candidate) != nil && got != "/safe-default" {
			t.Fatalf("invalid candidate %q produced %q", candidate, got)
		}
	})
}

func TestFormPageRequiresBoundCookieAndEscapesErrorState(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	target := dependencies.flow.beginResult.TransactionID
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+FormLoginPath+"?target_to="+target+"&error=false",
		nil,
	)
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()

	mount(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Invalid login or password") ||
		!strings.Contains(recorder.Body.String(), `name="target" value="`+target+`"`) {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	requireSecurityHeaders(t, recorder.Header())

	headRequest := request.Clone(context.Background())
	headRequest.Method = http.MethodHead
	headRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(headRecorder, headRequest)
	if headRecorder.Code != http.StatusOK || headRecorder.Body.Len() != 0 {
		t.Fatalf("HEAD status = %d body bytes = %d", headRecorder.Code, headRecorder.Body.Len())
	}
}

func TestFormPageUsesSelfContainedCurrentPresentation(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+FormLoginPath+"?target_to="+dependencies.flow.beginResult.TransactionID,
		nil,
	)
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()

	mount(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{
		"<title>Elitea login</title>",
		`data-auth-form="form"`,
		`class="card card-signin"`,
		`class="form-control"`,
		`action="/auth/form/authorize"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("login page is missing current Form marker %q", marker)
		}
	}
	for _, forbidden := range []string{
		"<link", " src=", "http://", "https://", "highly-sensitive-password",
		// No pack: no brand stylesheet and no logo image; the built-in logo
		// is inline SVG.
		"<style></style>", `class="brand-logo"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("login page contains forbidden external or secret material %q", forbidden)
		}
	}

	styleStart := strings.Index(body, "<style>")
	styleEnd := strings.Index(body, "</style>")
	if styleStart < 0 || styleEnd <= styleStart {
		t.Fatalf("login page does not contain one embedded style block")
	}
	style := body[styleStart+len("<style>") : styleEnd]
	digest := sha256.Sum256([]byte(style))
	wantSource := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src "+wantSource) || strings.Contains(csp, "'unsafe-inline'") {
		t.Fatalf("CSP = %q, want exact embedded-style hash %s", csp, wantSource)
	}
	assertScriptPinned(t, body, csp)
}

func TestFormPageRejectsMissingCookieWithoutRendering(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+FormLoginPath+"?target_to="+dependencies.flow.beginResult.TransactionID,
		nil,
	)
	recorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "<form") {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestFormAuthorizeAdmitsBeforeVerificationAndRotatesCookie(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	form := url.Values{
		"target":   {dependencies.flow.beginResult.TransactionID},
		"login":    {"admin"},
		"password": {"highly-sensitive-password"},
	}.Encode()
	request := httptest.NewRequest(http.MethodPost, BasePath+FormAuthorizePath, strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()

	mount(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != dependencies.flow.completeResult.ReturnTarget {
		t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if strings.Join(*dependencies.events, ",") != "client-key,admit,complete" {
		t.Fatalf("events = %v", *dependencies.events)
	}
	if dependencies.admitter.attempt.LoginDigest != sha256.Sum256([]byte("admin")) ||
		dependencies.admitter.attempt.ClientKey != "client-7" ||
		dependencies.admitter.attempt.Stage != BrowserAttemptFormCredential {
		t.Fatalf("attempt = %+v", dependencies.admitter.attempt)
	}
	if dependencies.flow.completeRequest.SessionID != dependencies.flow.beginResult.SessionID ||
		dependencies.flow.completeRequest.Provider != "form" {
		t.Fatalf("complete request = %+v", dependencies.flow.completeRequest)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != CookieValuePrefix+dependencies.flow.completeResult.SessionID ||
		cookies[0].Value == CookieValuePrefix+dependencies.flow.beginResult.SessionID {
		t.Fatalf("rotated cookies = %+v", cookies)
	}
	if strings.Contains(recorder.Body.String(), "highly-sensitive-password") {
		t.Fatal("response leaked submitted password")
	}
}

func TestFormLifecycleAcrossRealHTTPAndApplicationBoundaries(t *testing.T) {
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
	provider, err := browserapp.NewFormProvider([]byte(`{"users":[{
		"login":"admin",
		"password":"secret",
		"attributes":{"email":"admin@example.test","groups":["editor"]}
	}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "centry_auth_session", Secure: true,
		SameSite: http.SameSiteLaxMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]string, 0, 2)
	handler, err := NewHandler(
		flow,
		provider,
		&attemptAdmitterStub{events: &events},
		&clientKeyResolverStub{events: &events, key: "client-7"},
		cookies,
		Config{DefaultLoginTarget: "/", DefaultLogoutTarget: "/"},
	)
	if err != nil {
		t.Fatal(err)
	}

	beginRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(
		beginRecorder,
		httptest.NewRequest(http.MethodGet, BasePath+LoginPath+"?target_to=%2Fafter", nil),
	)
	if beginRecorder.Code != http.StatusFound {
		t.Fatalf("begin status = %d", beginRecorder.Code)
	}
	beginCookies := beginRecorder.Result().Cookies()
	if len(beginCookies) != 1 {
		t.Fatalf("begin cookies = %d, want 1", len(beginCookies))
	}
	loginLocation, err := url.Parse(beginRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	transactionID := loginLocation.Query().Get("target_to")
	if browserflow.ValidateTransactionID(transactionID) != nil {
		t.Fatalf("transaction ID = %q", transactionID)
	}

	form := url.Values{
		"target": {transactionID}, "login": {"admin"}, "password": {"secret"},
	}.Encode()
	authorizeRequest := httptest.NewRequest(
		http.MethodPost,
		BasePath+FormAuthorizePath,
		strings.NewReader(form),
	)
	authorizeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorizeRequest.AddCookie(beginCookies[0])
	authorizeRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(authorizeRecorder, authorizeRequest)
	if authorizeRecorder.Code != http.StatusFound || authorizeRecorder.Header().Get("Location") != "/after" {
		t.Fatalf("authorize status = %d location = %q", authorizeRecorder.Code, authorizeRecorder.Header().Get("Location"))
	}
	authenticatedCookies := authorizeRecorder.Result().Cookies()
	if len(authenticatedCookies) != 1 || authenticatedCookies[0].Value == beginCookies[0].Value {
		t.Fatalf("authenticated cookies = %+v", authenticatedCookies)
	}
	if len(transactions.records) != 0 || len(sessions.records) != 1 {
		t.Fatalf("transactions = %d sessions = %d", len(transactions.records), len(sessions.records))
	}
	if provisioner.request.Assertion.Provider != browserapp.FormProviderName ||
		provisioner.request.Assertion.ProviderReference != "admin" ||
		provisioner.request.Assertion.Email != "admin@example.test" {
		t.Fatalf("provision request = %+v", provisioner.request)
	}

	authorizeSessionRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	authorizeSessionRequest.AddCookie(authenticatedCookies[0])
	authenticatedSessionID, err := cookies.Read(authorizeSessionRequest)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := flow.Authorize(context.Background(), authenticatedSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Principal.UserID != "71" || authorization.Provider != browserapp.FormProviderName ||
		!json.Valid(authorization.ProviderAttributes) {
		t.Fatalf("authorization = %+v", authorization)
	}

	publicPolicy, err := forwardapp.NewPublicPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(panicCoreCredential(t), flow, publicPolicy)
	if err != nil {
		t.Fatal(err)
	}
	proxyResolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
		PublicOrigin:             "https://elitea.example.test",
		IdentityProjectionSecret: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	core, err := NewCoreHandler(kernel, proxyResolver, cookies, CoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	forwardRequest := coreRequest("/auth/check?target=rpc")
	forwardRequest.AddCookie(authenticatedCookies[0])
	forwardRecorder := httptest.NewRecorder()
	core.ServeHTTP(forwardRecorder, forwardRequest)
	if forwardRecorder.Code != http.StatusOK || forwardRecorder.Header().Get("X-Auth-Type") != "user" ||
		forwardRecorder.Header().Get("X-Auth-ID") != "71" ||
		forwardRecorder.Header().Get("X-Auth-Reference") != "-" {
		t.Fatalf("edge-auth status=%d headers=%v", forwardRecorder.Code, forwardRecorder.Header())
	}

	restartRequest := httptest.NewRequest(http.MethodGet, BasePath+LoginPath+"?target_to=%2Fagain", nil)
	restartRequest.AddCookie(authenticatedCookies[0])
	restartRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(restartRecorder, restartRequest)
	if restartRecorder.Code != http.StatusFound || len(sessions.records) != 1 {
		t.Fatalf("restart status = %d sessions = %d", restartRecorder.Code, len(sessions.records))
	}
	if _, present := sessions.records[authenticatedSessionID]; present {
		t.Fatal("starting a new login left the prior authenticated session usable")
	}
	restartedCookies := restartRecorder.Result().Cookies()
	if len(restartedCookies) != 1 || restartedCookies[0].Value == authenticatedCookies[0].Value {
		t.Fatalf("restarted cookies = %+v", restartedCookies)
	}

	logoutRequest := httptest.NewRequest(http.MethodGet, BasePath+FormLogoutPath+"?target_to=%2Fdone", nil)
	logoutRequest.AddCookie(restartedCookies[0])
	logoutRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(logoutRecorder, logoutRequest)
	if logoutRecorder.Code != http.StatusFound || logoutRecorder.Header().Get("Location") != "/done" ||
		len(sessions.records) != 0 {
		t.Fatalf("logout status = %d location = %q sessions = %d", logoutRecorder.Code, logoutRecorder.Header().Get("Location"), len(sessions.records))
	}
}

func TestFormAuthorizeRejectsMalformedBodiesBeforeAdmission(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "wrong content type", contentType: "application/json", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "missing field", contentType: "application/x-www-form-urlencoded", body: "login=admin&password=secret", wantStatus: http.StatusBadRequest},
		{name: "duplicate login", contentType: "application/x-www-form-urlencoded", body: "target=x&login=a&login=b&password=s", wantStatus: http.StatusBadRequest},
		{name: "unknown field", contentType: "application/x-www-form-urlencoded", body: "target=x&login=a&password=s&other=x", wantStatus: http.StatusBadRequest},
		{name: "body too large", contentType: "application/x-www-form-urlencoded", body: strings.Repeat("x", int(DefaultMaxFormBodyBytes+1)), wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, dependencies := newTestHandler(t)
			request := httptest.NewRequest(http.MethodPost, BasePath+FormAuthorizePath, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
			recorder := httptest.NewRecorder()
			mount(handler).ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if dependencies.admitter.calls != 0 || dependencies.flow.completeCalls != 0 {
				t.Fatalf("malformed input reached dependencies: %+v", dependencies)
			}
		})
	}
}

func TestFormAuthorizeRateLimitPrecedesPasswordVerification(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	dependencies.admitter.err = ErrAttemptLimited
	dependencies.admitter.retryAfter = 1500 * time.Millisecond
	recorder := authorize(t, handler, dependencies, "known-user", "secret-canary")
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "2" {
		t.Fatalf("status = %d Retry-After = %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if dependencies.flow.completeCalls != 0 {
		t.Fatal("rate-limited request reached password verification")
	}
	if strings.Contains(recorder.Body.String(), "known-user") || strings.Contains(recorder.Body.String(), "secret-canary") {
		t.Fatalf("response leaked credential input: %q", recorder.Body.String())
	}
}

func TestFormAuthorizeMapsCredentialAndDependencyFailuresGenerically(t *testing.T) {
	t.Run("credential", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = browserapp.ErrUnauthenticated
		dependencies.flow.completeResult = browserapp.CompleteResult{}
		recorder := authorize(t, handler, dependencies, "unknown-user", "secret-canary")
		if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != BasePath+LoginPath+"?error=true" {
			t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
		}
		if strings.Contains(recorder.Body.String(), "unknown-user") || strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("response leaked credential input: %q", recorder.Body.String())
		}
	})

	// ADR-0025 §3.9: a wrong password keeps the return target, so a typo on
	// the way to a deep link (or a native sign-in's consent page) does not
	// strand the user on the default landing page.
	t.Run("credential keeps the return target", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = browserapp.ErrUnauthenticated
		dependencies.flow.completeResult = browserapp.CompleteResult{
			ReturnTarget: "/api/v2/auth/native/authorize/continue?request=abc",
		}
		recorder := authorize(t, handler, dependencies, "unknown-user", "secret-canary")
		want := BasePath + LoginPath + "?error=true&target_to=%2Fapi%2Fv2%2Fauth%2Fnative%2Fauthorize%2Fcontinue%3Frequest%3Dabc"
		if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != want {
			t.Fatalf("status = %d location = %q, want %q", recorder.Code, recorder.Header().Get("Location"), want)
		}
	})

	t.Run("credential drops a non-canonical target", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = browserapp.ErrUnauthenticated
		dependencies.flow.completeResult = browserapp.CompleteResult{ReturnTarget: "https://evil.example/"}
		recorder := authorize(t, handler, dependencies, "unknown-user", "secret-canary")
		if recorder.Header().Get("Location") != BasePath+LoginPath+"?error=true" {
			t.Fatalf("location = %q", recorder.Header().Get("Location"))
		}
	})

	t.Run("dependency", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = errors.New("store unavailable for secret-canary")
		recorder := authorize(t, handler, dependencies, "admin", "secret-canary")
		if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
		}
	})
}

// TestFormAuthorizeRecoversFromStaleFormInsteadOfDeadEnding gates the fix for
// the login dead end.
//
// THE DEFECT. An unauthenticated visit to /app/ asks for the login page twice:
// the SPA router redirects, and the first API call answers 302 to the same
// place. Each GET of /auth/login mints a new session cookie and a new
// transaction, so the two begins race and the last Set-Cookie wins. The form on
// screen then carries a transaction the server no longer accepts. Complete
// answers ErrTransactionRejected.
//
// Before the fix that produced a bare text/plain "Bad Request" page with no
// form and no link. A normal browser could not sign in at all. The two cases
// below must both put the browser back on a fresh login form.
func TestFormAuthorizeRecoversFromStaleFormInsteadOfDeadEnding(t *testing.T) {
	t.Run("stale transaction", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = browserapp.ErrTransactionRejected
		recorder := authorize(t, handler, dependencies, "admin", "secret-canary")
		if recorder.Code != http.StatusFound ||
			recorder.Header().Get("Location") != BasePath+LoginPath+"?error=true" {
			t.Fatalf("status = %d location = %q, want 302 to the login page",
				recorder.Code, recorder.Header().Get("Location"))
		}
		if strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("response leaked credential input: %q", recorder.Body.String())
		}
	})

	t.Run("session cookie gone", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		form := url.Values{
			"target":   {dependencies.flow.beginResult.TransactionID},
			"login":    {"admin"},
			"password": {"secret-canary"},
		}.Encode()
		request := httptest.NewRequest(
			http.MethodPost,
			BasePath+FormAuthorizePath,
			strings.NewReader(form),
		)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// No session cookie: it expired between the render and this POST.
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusFound ||
			recorder.Header().Get("Location") != BasePath+LoginPath+"?error=true" {
			t.Fatalf("status = %d location = %q, want 302 to the login page",
				recorder.Code, recorder.Header().Get("Location"))
		}
		if dependencies.flow.completeCalls != 0 {
			t.Fatal("a request with no session cookie reached the authentication flow")
		}
	})

	t.Run("malformed request stays a client error", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.completeErr = browserapp.ErrInvalidRequest
		recorder := authorize(t, handler, dependencies, "admin", "secret")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
		}
	})
}

func TestFormAuthorizeRejectsNonRotatedSession(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	dependencies.flow.completeResult.SessionID = dependencies.flow.beginResult.SessionID
	recorder := authorize(t, handler, dependencies, "admin", "secret")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatalf("cookies = %+v, want exact deletion", cookies)
	}
}

func TestFormAuthorizeRejectsUnsafeReturnTarget(t *testing.T) {
	for _, target := range []string{
		"https://attacker.example/path",
		"//attacker.example/path",
		`/\attacker.example`,
		`/%5cattacker.example`,
		`/%2fattacker.example`,
		"/ok%0d%0aLocation:https://attacker.example",
	} {
		t.Run(target, func(t *testing.T) {
			handler, dependencies := newTestHandler(t)
			dependencies.flow.completeResult.ReturnTarget = target
			recorder := authorize(t, handler, dependencies, "admin", "secret")
			if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Location") != "" {
				t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
			}
			if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
				t.Fatalf("cookies = %+v, want exact deletion", cookies)
			}
		})
	}
}

func TestLogoutPreservesProviderRouteAndFailsClosedOnUncertainRevocation(t *testing.T) {
	t.Run("provider dispatch", func(t *testing.T) {
		handler, _ := newTestHandler(t)
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, BasePath+LogoutPath, nil))
		if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != BasePath+FormLogoutPath+"?target_to=%2Flogout-default" {
			t.Fatalf("status = %d location = %q", recorder.Code, recorder.Header().Get("Location"))
		}
	})

	t.Run("success", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		request := httptest.NewRequest(http.MethodGet, BasePath+FormLogoutPath+"?target_to=%2Fafter", nil)
		request.AddCookie(sessionCookie(dependencies.flow.completeResult.SessionID))
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/after" ||
			dependencies.flow.logoutID != dependencies.flow.completeResult.SessionID {
			t.Fatalf("status = %d location = %q logout ID = %q", recorder.Code, recorder.Header().Get("Location"), dependencies.flow.logoutID)
		}
		if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
			t.Fatalf("cookies = %+v", cookies)
		}
	})

	t.Run("unsafe target falls back", func(t *testing.T) {
		for _, target := range []string{
			"https://attacker.example/path",
			"//attacker.example/path",
			`/\attacker.example`,
			`/%5cattacker.example`,
			`/%2fattacker.example`,
			"/ok%0d%0aLocation:https://attacker.example",
		} {
			handler, _ := newTestHandler(t)
			request := httptest.NewRequest(
				http.MethodGet,
				BasePath+FormLogoutPath+"?target_to="+url.QueryEscape(target),
				nil,
			)
			recorder := httptest.NewRecorder()
			mount(handler).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/logout-default" {
				t.Fatalf("target = %q status = %d location = %q", target, recorder.Code, recorder.Header().Get("Location"))
			}
		}
	})

	t.Run("revocation dependency outage", func(t *testing.T) {
		handler, dependencies := newTestHandler(t)
		dependencies.flow.logoutErr = errors.New("store unavailable for secret-canary")
		request := httptest.NewRequest(http.MethodGet, BasePath+FormLogoutPath, nil)
		request.AddCookie(sessionCookie(dependencies.flow.completeResult.SessionID))
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Location") != "" ||
			strings.Contains(recorder.Body.String(), "secret-canary") {
			t.Fatalf("status = %d location = %q body = %q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
		}
		if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
			t.Fatalf("cookies = %+v", cookies)
		}
	})
}

func TestOptionsAreExplicitAndDoNotReachDependencies(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	for path, allow := range map[string]string{
		LoginPath:         "GET, HEAD, OPTIONS",
		FormAuthorizePath: "POST, OPTIONS",
	} {
		recorder := httptest.NewRecorder()
		mount(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodOptions, BasePath+path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Allow") != allow {
			t.Fatalf("%s: status = %d Allow = %q", path, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
	if dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 || dependencies.flow.logoutCalls != 0 {
		t.Fatal("OPTIONS reached browser authentication flow")
	}
}

func TestUnsupportedFormMethodPreservesCurrentBadRequestStatus(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	recorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPut, BasePath+FormAuthorizePath, nil),
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 ||
		dependencies.flow.logoutCalls != 0 || dependencies.admitter.calls != 0 {
		t.Fatal("unsupported method reached browser authentication dependencies")
	}
}

func TestNewHandlerRejectsIncompleteOrUnsafeConfiguration(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	_ = handler
	valid := Config{DefaultLoginTarget: "/", DefaultLogoutTarget: "/", MaxFormBodyBytes: 4096}
	tests := []struct {
		name       string
		flow       Flow
		form       *browserapp.FormProvider
		attempts   AttemptAdmitter
		clientKeys ClientKeyResolver
		cookies    *CookiePolicy
		config     Config
	}{
		{name: "missing flow", form: dependencies.form, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing form", flow: dependencies.flow, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing attempts", flow: dependencies.flow, form: dependencies.form, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: valid},
		{name: "missing client policy", flow: dependencies.flow, form: dependencies.form, attempts: dependencies.admitter, cookies: dependencies.cookies, config: valid},
		{name: "external target", flow: dependencies.flow, form: dependencies.form, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: Config{DefaultLoginTarget: "https://attacker.example", DefaultLogoutTarget: "/", MaxFormBodyBytes: 4096}},
		{name: "unbounded body", flow: dependencies.flow, form: dependencies.form, attempts: dependencies.admitter, clientKeys: dependencies.resolver, cookies: dependencies.cookies, config: Config{DefaultLoginTarget: "/", DefaultLogoutTarget: "/", MaxFormBodyBytes: maxMaxFormBodyBytes + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHandler(test.flow, test.form, test.attempts, test.clientKeys, test.cookies, test.config); !errors.Is(err, ErrInvalidHandlerConfiguration) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidHandlerConfiguration)
			}
		})
	}
}

type testDependencies struct {
	flow     *flowStub
	form     *browserapp.FormProvider
	admitter *attemptAdmitterStub
	resolver *clientKeyResolverStub
	cookies  *CookiePolicy
	events   *[]string
}

func newTestHandler(t *testing.T) (*Handler, *testDependencies) {
	t.Helper()
	events := make([]string, 0, 4)
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
	form, err := browserapp.NewFormProvider([]byte(`{"users":[{"login":"admin","password":"highly-sensitive-password"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	admitter := &attemptAdmitterStub{events: &events}
	resolver := &clientKeyResolverStub{events: &events, key: "client-7"}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(flow, form, admitter, resolver, cookies, Config{
		DefaultLoginTarget:  "/safe-default",
		DefaultLogoutTarget: "/logout-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, &testDependencies{
		flow: flow, form: form, admitter: admitter, resolver: resolver,
		cookies: cookies, events: &events,
	}
}

func mount(handler *Handler) http.Handler {
	router := chi.NewRouter()
	router.Mount(BasePath, handler.Routes())
	return router
}

func authorize(
	t *testing.T,
	handler *Handler,
	dependencies *testDependencies,
	login string,
	password string,
) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"target":   {dependencies.flow.beginResult.TransactionID},
		"login":    {login},
		"password": {password},
	}.Encode()
	request := httptest.NewRequest(http.MethodPost, BasePath+FormAuthorizePath, strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(recorder, request)
	return recorder
}

func sessionCookie(sessionID string) *http.Cookie {
	return &http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID}
}

func requireSecurityHeaders(t *testing.T, headers http.Header) {
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
	if !strings.Contains(headers.Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatalf("CSP = %q", headers.Get("Content-Security-Policy"))
	}
}

type flowStub struct {
	events           *[]string
	beginRequest     browserapp.BeginRequest
	beginResult      browserapp.BeginResult
	beginErr         error
	beginCalls       int
	completeRequest  browserapp.CompleteRequest
	completeResult   browserapp.CompleteResult
	completeVerifier browserapp.AssertionVerifier
	completeErr      error
	completeCalls    int
	logoutID         string
	logoutErr        error
	logoutCalls      int
}

func (stub *flowStub) Begin(_ context.Context, request browserapp.BeginRequest) (browserapp.BeginResult, error) {
	stub.beginCalls++
	stub.beginRequest = request
	*stub.events = append(*stub.events, "begin")
	return stub.beginResult, stub.beginErr
}

func (stub *flowStub) Complete(
	_ context.Context,
	request browserapp.CompleteRequest,
	verifier browserapp.AssertionVerifier,
) (browserapp.CompleteResult, error) {
	stub.completeCalls++
	stub.completeRequest = request
	stub.completeVerifier = verifier
	*stub.events = append(*stub.events, "complete")
	return stub.completeResult, stub.completeErr
}

func (stub *flowStub) Logout(_ context.Context, sessionID string) (browserapp.LogoutContext, error) {
	stub.logoutCalls++
	stub.logoutID = sessionID
	*stub.events = append(*stub.events, "logout")
	return browserapp.LogoutContext{}, stub.logoutErr
}

type attemptAdmitterStub struct {
	events     *[]string
	attempt    BrowserAttempt
	retryAfter time.Duration
	err        error
	calls      int
}

func (stub *attemptAdmitterStub) Admit(_ context.Context, attempt BrowserAttempt) (time.Duration, error) {
	stub.calls++
	stub.attempt = attempt
	*stub.events = append(*stub.events, "admit")
	return stub.retryAfter, stub.err
}

type clientKeyResolverStub struct {
	events *[]string
	key    string
	err    error
}

func (stub *clientKeyResolverStub) ResolveClientKey(*http.Request) (string, error) {
	*stub.events = append(*stub.events, "client-key")
	return stub.key, stub.err
}

type componentSessionStore struct {
	records map[string]sessionstate.State
	next    byte
}

func (store *componentSessionStore) Create(_ context.Context, state sessionstate.State) (string, error) {
	store.next++
	id := canonicalSessionID(store.next)
	store.records[id] = cloneComponentSessionState(state)
	return id, nil
}

func (store *componentSessionStore) Read(_ context.Context, id string) (sessionstate.State, error) {
	state, present := store.records[id]
	if !present {
		return sessionstate.State{}, sessionstate.ErrNotFound
	}
	return cloneComponentSessionState(state), nil
}

func (store *componentSessionStore) RotateAndReplace(
	_ context.Context,
	id string,
	replacement sessionstate.State,
) (string, error) {
	if _, present := store.records[id]; !present {
		return "", sessionstate.ErrNotFound
	}
	store.next++
	replacementID := canonicalSessionID(store.next)
	store.records[replacementID] = cloneComponentSessionState(replacement)
	delete(store.records, id)
	return replacementID, nil
}

func (store *componentSessionStore) ConsumeForLogout(
	_ context.Context,
	id string,
) (sessionstate.State, error) {
	state, present := store.records[id]
	if !present {
		return sessionstate.State{}, nil
	}
	delete(store.records, id)
	return cloneComponentSessionState(state), nil
}

func (store *componentSessionStore) Delete(_ context.Context, id string) error {
	delete(store.records, id)
	return nil
}

type componentTransactionStore struct {
	records map[string]browserflow.Transaction
	next    byte
}

func (store *componentTransactionStore) Create(
	_ context.Context,
	transaction browserflow.Transaction,
) (string, error) {
	store.next++
	id := canonicalSessionID(128 + store.next)
	store.records[id] = transaction
	return id, nil
}

func (store *componentTransactionStore) Consume(
	_ context.Context,
	id string,
	provider string,
	originatingSessionID string,
) (browserflow.Transaction, error) {
	transaction, present := store.records[id]
	if !present || transaction.Provider != provider ||
		transaction.OriginatingSessionID != originatingSessionID {
		return browserflow.Transaction{}, browserflow.ErrTransactionRejected
	}
	delete(store.records, id)
	return transaction, nil
}

type componentProvisioner struct {
	request identity.ProvisionRequest
	result  identity.ProvisionResult
}

func (provisioner *componentProvisioner) Provision(
	_ context.Context,
	request identity.ProvisionRequest,
) (identity.ProvisionResult, error) {
	provisioner.request = request
	return provisioner.result, nil
}

type componentPrincipalValidator struct{}

func (componentPrincipalValidator) ValidatePrincipal(_ context.Context, principal auth.User) (auth.User, error) {
	principal.Email = "admin@example.test"
	return principal, nil
}

func cloneComponentSessionState(state sessionstate.State) sessionstate.State {
	if state.Provider != nil {
		provider := *state.Provider
		state.Provider = &provider
	}
	if state.Expiration != nil {
		expiration := state.Expiration.UTC()
		state.Expiration = &expiration
	}
	if state.UserID != nil {
		userID := *state.UserID
		state.UserID = &userID
	}
	state.ProviderAttributes = append(json.RawMessage(nil), state.ProviderAttributes...)
	return state
}

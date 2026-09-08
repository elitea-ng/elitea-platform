package deepwiki_test

// The facade, tested against a REAL mTLS provider rather than a stubbed
// transport.
//
// A fake transport would pass while the client certificate, the CA and the
// server name were all wrong, because nothing would ever complete a handshake.
// Everything this package exists to get right — the hop authenticates, the
// caller's credentials do not travel, the identity is this service's and not
// the client's — is only observable when a real peer terminates the connection
// and looks at what arrived.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

const identitySecret = "shared-with-the-provider"

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type principalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (f principalValidatorFunc) ValidatePrincipal(ctx context.Context, user auth.User) (auth.User, error) {
	return f(ctx, user)
}

type forwardedPeerVerifierFunc func(*http.Request) error

func (f forwardedPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(ctx context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
	return f(ctx, user, mode, projectID)
}

// received is what the provider saw.
type received struct {
	method string
	path   string
	host   string
	header http.Header
	body   string
}

// provider starts an mTLS server that records the requests it receives, and
// returns a Config wired to reach it.
func provider(t *testing.T) (*[]received, deepwiki.Config) {
	t.Helper()

	ca, caKey := authority(t, "elitea-test-ca")
	serverCert := issue(t, ca, caKey, "deepwiki.internal", x509.ExtKeyUsageServerAuth)
	clientCert := issue(t, ca, caKey, "elitea-main", x509.ExtKeyUsageClientAuth)

	pool := x509.NewCertPool()
	pool.AddCert(ca)

	var log []received
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				_, _ = r.Body.Read(body)
			}
			log = append(log, received{
				method: r.Method,
				path:   r.URL.Path,
				host:   r.Host,
				header: r.Header.Clone(),
				body:   string(body),
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		// The provider refuses anything it cannot authenticate. A facade that
		// presents no certificate must FAIL here, not be waved through.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	caFile := filepath.Join(dir, "ca.crt")
	writePEM(t, certFile, "CERTIFICATE", clientCert.Certificate[0])
	writeKey(t, keyFile, clientCert.PrivateKey.(*ecdsa.PrivateKey))
	writePEM(t, caFile, "CERTIFICATE", ca.Raw)

	return &log, deepwiki.Config{
		Enabled:        true,
		BaseURL:        server.URL,
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
		// httptest serves on 127.0.0.1; the certificate names the service.
		ServerName:     "deepwiki.internal",
		IdentitySecret: identitySecret,
		Timeout:        10 * time.Second,

		CallbackBaseURL:  "https://elitea.test",
		CallbackTokenTTL: time.Hour,
		GitEgress:        deepwiki.ParseGitEgressPolicy("github.com,api.github.com"),
	}
}

func authority(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeKey(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, path, "EC PRIVATE KEY", der)
}

// route builds the facade with a caller holding exactly `granted`.
func route(t *testing.T, cfg deepwiki.Config, granted ...string) *deepwiki.Route {
	t.Helper()
	built, err := deepwiki.NewRoute(cfg, authConfig(), resolver(granted...),
		testCredentialResolver(t), nil, &recordingMinter{}, slog.New(
			slog.NewTextHandler(&strings.Builder{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func authConfig() apimw.AuthConfig {
	return apimw.AuthConfig{
		PrincipalValidator: principalValidatorFunc(
			func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
		ForwardedIdentityVerifier: forwardedPeerVerifierFunc(
			func(*http.Request) error { return nil }),
	}
}

func resolver(granted ...string) auth.PermissionResolver {
	return permissionResolverFunc(
		func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: granted}, nil
		})
}

func call(t *testing.T, handler http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// ---------------------------------------------------------------------------
// the hop
// ---------------------------------------------------------------------------

// Each facade path maps to exactly one provider path. Getting this wrong is
// invisible in a unit test of the path builder alone, because the router is
// what supplies the segments.
func TestEachFacadePathMapsToItsProviderPath(t *testing.T) {
	log, cfg := provider(t)
	handler := route(t, cfg,
		deepwiki.ReadPermission, deepwiki.GeneratePermission)

	cases := []struct {
		method string
		facade string
		body   string
		want   string
	}{
		{http.MethodGet, "/api/v2/deepwiki/slots/7", "", "/slots"},
		{
			http.MethodPost,
			"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
			`{"configuration":{"parameters":{"code_toolkit":42}}}`,
			"/tools/Wikis/generate_wiki/invoke",
		},
		{
			http.MethodGet,
			"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc",
			"",
			"/tools/Wikis/generate_wiki/invocations/abc",
		},
		{
			http.MethodDelete,
			"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc",
			"",
			"/tools/Wikis/generate_wiki/invocations/abc",
		},
	}

	for _, testCase := range cases {
		response := call(t, handler, testCase.method, testCase.facade, testCase.body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d body %s",
				testCase.method, testCase.facade, response.Code, response.Body.String())
		}
		last := (*log)[len(*log)-1]
		if last.path != testCase.want || last.method != testCase.method {
			t.Fatalf("%s %s reached the provider as %s %s, want %s",
				testCase.method, testCase.facade, last.method, last.path, testCase.want)
		}
	}

	// A body must arrive. The facade REWRITES an invoke rather than passing it
	// through (see credentials_test.go for what it becomes), so this asserts
	// only that a payload crossed the hop — a proxy that mapped the path and
	// dropped the body would satisfy every other assertion above.
	if !strings.Contains((*log)[1].body, "github_configuration") {
		t.Fatalf("no rewritten body reached the provider: %q", (*log)[1].body)
	}
}

// The identity is this service's, signed here, and the client cannot
// contribute to it — even by sending the headers itself.
func TestTheProviderReceivesASignedIdentityTheClientCannotInfluence(t *testing.T) {
	log, cfg := provider(t)
	handler := route(t, cfg, deepwiki.ReadPermission)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/deepwiki/slots/7", nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	// A caller trying to be someone else, in every field of the scheme.
	request.Header.Set(llmproxy.HeaderProjectID, "999")
	request.Header.Set(llmproxy.HeaderUserID, "1")
	request.Header.Set(llmproxy.HeaderTenantID, "other-tenant")
	request.Header.Set(llmproxy.HeaderSignature, "sha256=deadbeef")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	arrived := (*log)[0].header

	if got := arrived.Get(llmproxy.HeaderProjectID); got != "7" {
		t.Fatalf("project id %q — the client's 999 was not replaced by the path's 7", got)
	}
	if got := arrived.Get(llmproxy.HeaderUserID); got != "11" {
		t.Fatalf("user id %q — the client's spoofed 1 survived", got)
	}
	if got := arrived.Get(llmproxy.HeaderTenantID); got != "" {
		t.Fatalf("tenant %q — nothing in this request carried one", got)
	}

	// The signature must verify under the canonical string, which is what the
	// provider's Python verifier recomputes. Recomputed here rather than
	// compared to a recorded constant, so a change to the scheme on either
	// side shows up as a mismatch.
	mac := hmac.New(sha256.New, []byte(identitySecret))
	mac.Write([]byte("v1\n7\n11\n"))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := arrived.Get(llmproxy.HeaderSignature); got != want {
		t.Fatalf("signature %q, want %q", got, want)
	}
}

// The caller's own credentials stay on this side. The provider authenticates
// the hop, not the end user, and has no use for a platform bearer token.
func TestTheCallersCredentialsDoNotReachTheProvider(t *testing.T) {
	log, cfg := provider(t)
	handler := route(t, cfg, deepwiki.ReadPermission)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/deepwiki/slots/7", nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.Header.Set("Authorization", "Bearer platform-token")
	request.Header.Set("Cookie", "elitea_session=secret")
	request.Header.Set("X-Secret", "legacy-shared-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	for _, header := range []string{"Authorization", "Cookie", "X-Secret"} {
		if got := (*log)[0].header.Get(header); got != "" {
			t.Fatalf("%s reached the provider as %q", header, got)
		}
	}

	// The provider is addressed by ITS OWN name, not the caller's. A proxy
	// that forwards the inbound Host sends a peer a hostname it does not
	// serve, and the failure surfaces as a routing error rather than as a
	// misconfigured proxy.
	if (*log)[0].host == request.Host {
		t.Fatalf("the caller's Host (%q) was forwarded to the provider", request.Host)
	}
}

// ---------------------------------------------------------------------------
// authorization
// ---------------------------------------------------------------------------

// Reading capacity and starting a generation are different grants, and the two
// methods that share the invocation path do not share one either.
func TestReadingIsNotGenerating(t *testing.T) {
	log, cfg := provider(t)
	reader := route(t, cfg, deepwiki.ReadPermission)

	if response := call(t, reader, http.MethodGet,
		"/api/v2/deepwiki/slots/7", ""); response.Code != http.StatusOK {
		t.Fatalf("a viewer could not see capacity: %d %s",
			response.Code, response.Body.String())
	}
	if response := call(t, reader, http.MethodGet,
		"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc", ""); response.Code != http.StatusOK {
		t.Fatalf("a viewer could not follow a generation: %d", response.Code)
	}

	forwarded := len(*log)

	if response := call(t, reader, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke", `{}`); response.Code != http.StatusForbidden {
		t.Fatalf("the read grant started a generation: %d", response.Code)
	}
	// Cancelling another member's run is a mutation of shared state, not an
	// observation of it, so it is grouped with generate and not with the poll
	// it shares a path with.
	if response := call(t, reader, http.MethodDelete,
		"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc", ""); response.Code != http.StatusForbidden {
		t.Fatalf("the read grant cancelled a generation: %d", response.Code)
	}

	if len(*log) != forwarded {
		t.Fatalf("a refused request still reached the provider (%d new)",
			len(*log)-forwarded)
	}
}

func TestAGeneratorMayStartAndCancel(t *testing.T) {
	log, cfg := provider(t)
	generator := route(t, cfg, deepwiki.ReadPermission, deepwiki.GeneratePermission)

	if response := call(t, generator, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42}}}`); response.Code != http.StatusOK {
		t.Fatalf("start refused: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, generator, http.MethodDelete,
		"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc", ""); response.Code != http.StatusOK {
		t.Fatalf("cancel refused: %d", response.Code)
	}
	if len(*log) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(*log))
	}
}

// An unauthenticated caller never reaches the provider. The gate order is
// authenticate, then resolve permissions, then proxy.
func TestAnUnauthenticatedCallerIsRefusedBeforeTheHop(t *testing.T) {
	log, cfg := provider(t)
	handler := route(t, cfg, deepwiki.ReadPermission)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/deepwiki/slots/7", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code == http.StatusOK {
		t.Fatal("an unauthenticated request was served")
	}
	if len(*log) != 0 {
		t.Fatalf("an unauthenticated request reached the provider")
	}
}

// A project id that is not a positive decimal is rejected before it reaches
// either the permission resolver or the provider.
func TestANonNumericProjectIsRefusedBeforeTheHop(t *testing.T) {
	log, cfg := provider(t)
	handler := route(t, cfg, deepwiki.ReadPermission)

	for _, projectID := range []string{"abc", "0", "-1", "7;drop", "01"} {
		response := call(t, handler, http.MethodGet,
			"/api/v2/deepwiki/slots/"+projectID, "")
		if response.Code == http.StatusOK {
			t.Fatalf("project id %q was served", projectID)
		}
	}
	if len(*log) != 0 {
		t.Fatalf("an invalid project id reached the provider")
	}
}

// ---------------------------------------------------------------------------
// composition
// ---------------------------------------------------------------------------

// A nil principal validator is the recurring shape of authentication bypass in
// this service: the route serves perfectly well without one, it just does not
// authenticate. Composition refuses instead.
func TestCompositionRefusesAHalfWiredAuthenticationChain(t *testing.T) {
	_, cfg := provider(t)

	full := authConfig()
	cases := map[string]apimw.AuthConfig{
		"no principal validator": {
			ForwardedIdentityVerifier: full.ForwardedIdentityVerifier,
		},
		"no forwarded identity verifier": {
			PrincipalValidator: full.PrincipalValidator,
		},
		"neither": {},
	}
	for name, broken := range cases {
		if _, err := deepwiki.NewRoute(cfg, broken, resolver(), testCredentialResolver(t), nil, &recordingMinter{}, nil); !errors.Is(err, deepwiki.ErrInvalidRoute) {
			t.Fatalf("%s was accepted: %v", name, err)
		}
	}

	if _, err := deepwiki.NewRoute(cfg, full, nil, testCredentialResolver(t), nil, &recordingMinter{}, nil); !errors.Is(err, deepwiki.ErrInvalidRoute) {
		t.Fatalf("a nil permission resolver was accepted: %v", err)
	}
	// The OIDC-only credential set — a token validator in place of the
	// forwarded-identity verifier — composes (ADR-0023 decision 5).
	oidcOnly := apimw.AuthConfig{PrincipalValidator: full.PrincipalValidator, Validator: stubTokenValidator{}, SessionSecret: "s"}
	if _, err := deepwiki.NewRoute(cfg, oidcOnly, resolver(), testCredentialResolver(t), nil, &recordingMinter{}, nil); err != nil {
		t.Fatalf("the OIDC-only credential set was refused: %v", err)
	}
}

// A plain-HTTP base URL cannot work — the provider serves nothing there — so
// it is a startup failure rather than four routes that fail per call.
func TestAPlainHTTPProviderURLIsRefusedAtComposition(t *testing.T) {
	_, cfg := provider(t)
	cfg.BaseURL = strings.Replace(cfg.BaseURL, "https://", "http://", 1)

	if _, err := deepwiki.NewRoute(cfg, authConfig(), resolver(), testCredentialResolver(t), nil, &recordingMinter{}, nil); !errors.Is(err, deepwiki.ErrInvalidProxy) {
		t.Fatalf("a plain-HTTP provider URL was accepted: %v", err)
	}
}

// A route that was never built answers rather than panicking: a mount that
// half-happened must not take the process down, and the zero value is exactly
// what the router's spec-conformance walk registers.
func TestAZeroRouteRefusesInsteadOfPanicking(t *testing.T) {
	response := call(t, &deepwiki.Route{}, http.MethodGet, "/api/v2/deepwiki/slots/7", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), "not enabled") {
		t.Fatalf("unhelpful body %q", response.Body.String())
	}

	var absent *deepwiki.Route
	response = call(t, absent, http.MethodGet, "/api/v2/deepwiki/slots/7", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil route: status %d, want 503", response.Code)
	}
}

// An unreachable provider is a 503 with a readable body, not a panic and not a
// 200 with an empty payload.
func TestAnUnreachableProviderIsAReadable503(t *testing.T) {
	_, cfg := provider(t)
	// A port nothing listens on. The certificates are still valid, so this
	// isolates reachability from the mTLS material.
	cfg.BaseURL = "https://127.0.0.1:1"

	handler := route(t, cfg, deepwiki.ReadPermission)
	response := call(t, handler, http.MethodGet, "/api/v2/deepwiki/slots/7", "")

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unreachable") {
		t.Fatalf("unhelpful body %q", response.Body.String())
	}
}

// The permission strings are load-bearing: migration 0106 grants exactly these
// two, and a rename here without one there is 403-for-everyone.
func TestThePermissionStringsMatchTheMigration(t *testing.T) {
	if deepwiki.ReadPermission != "models.applications.deepwiki.read" ||
		deepwiki.GeneratePermission != "models.applications.deepwiki.generate" ||
		deepwiki.Mode != auth.PermissionModeDefault {
		t.Fatalf("route constants drifted from shared/0106: read=%q generate=%q mode=%q",
			deepwiki.ReadPermission, deepwiki.GeneratePermission, deepwiki.Mode)
	}
}

// stubTokenValidator stands in for the LocalValidator an OIDC-only
// deployment reads APPLICATION_SECRET_KEY-signed tokens back with.
type stubTokenValidator struct{}

func (stubTokenValidator) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{ID: "7"}, nil
}

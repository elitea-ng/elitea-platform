package inventory_test

// The Inventory facade at its ROUTE SURFACE: which paths exist, which grant
// each one needs, what reaches the provider, and what a half-composed mount
// answers.
//
// sources_test.go covers the body rewrite and counts the calls that prove its
// order. This file covers everything AROUND it — the three paths, the two
// permissions, the identity the provider receives, the credentials it must not
// receive, and the four ways NewRoute can refuse. Those are the decisions a
// composition root makes, and the survey that started this work measured them
// at 52.8% covered: the shape where "the facade is mounted" and "the facade
// authenticates" cannot be told apart.

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

// silent is the logger every case here builds with: a facade that logged to
// the test's own output would bury the assertion that failed.
func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
}

// recordedRequest is what the provider saw.
type recordedRequest struct {
	method string
	path   string
	host   string
	header http.Header
}

// recordingProvider is an mTLS peer that records the whole request — the
// method, the path, the Host and every header — and a facade Config wired to
// reach it.
//
// A REAL PEER, not a stubbed transport, for the reason sources_test.go gives:
// a fake would pass while the certificate, the CA and the server name were all
// wrong, because nothing would ever complete a handshake. It is a second peer
// rather than that file's because provider() keeps only the bodies, and the
// header set is what this file asserts on.
func recordingProvider(t *testing.T) (*[]recordedRequest, facade.Config) {
	t.Helper()
	ca, caKey := authority(t, "elitea-test-ca")
	serverCert := issue(t, ca, caKey, "inventory.internal", x509.ExtKeyUsageServerAuth)
	clientCert := issue(t, ca, caKey, "elitea-main", x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	var seen []recordedRequest
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, recordedRequest{
				method: r.Method, path: r.URL.Path, host: r.Host, header: r.Header.Clone(),
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		// The provider refuses anything it cannot authenticate. A facade that
		// presented no certificate must FAIL here, not be waved through.
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

	return &seen, facade.Config{
		Enabled:        true,
		BaseURL:        server.URL,
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
		ServerName:     "inventory.internal",
		IdentitySecret: "shared-with-the-provider",
		Timeout:        10 * time.Second,
	}
}

// route composes the facade with a caller holding exactly `granted`, and no
// source expansion — the deployment shape values.yaml documents as
// "ELITEA_INVENTORY_CALLBACK_BASE_URL empty".
func route(t *testing.T, cfg facade.Config, granted ...string) *inventory.Route {
	t.Helper()
	built, err := inventory.NewRoute(cfg, authConfig(), permissions(granted...), nil, silent())
	if err != nil {
		t.Fatalf("compose the Inventory route: %v", err)
	}
	return built
}

func request(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-Auth-Type", "user")
	r.Header.Set("X-Auth-ID", "11")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	return response
}

// ---------------------------------------------------------------------------
// the three paths
// ---------------------------------------------------------------------------

// Each facade path maps to exactly one provider path. The router supplies the
// segments, so a unit test of the path builder alone cannot see this.
func TestEachFacadePathMapsToItsProviderPath(t *testing.T) {
	seen, cfg := recordingProvider(t)
	handler := route(t, cfg, inventory.ReadPermission, inventory.InvokePermission)

	cases := []struct{ method, facadePath, providerPath string }{
		{http.MethodGet, "/api/v2/inventory/slots/7", "/slots"},
		{
			http.MethodGet,
			"/api/v2/inventory/invocations/7/inventory/get_stats/abc",
			"/tools/inventory/get_stats/invocations/abc",
		},
		{
			http.MethodDelete,
			"/api/v2/inventory/invocations/7/inventory/get_stats/abc",
			"/tools/inventory/get_stats/invocations/abc",
		},
	}
	for _, testCase := range cases {
		response := request(t, handler, testCase.method, testCase.facadePath)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", testCase.method, testCase.facadePath,
				response.Code, response.Body.String())
		}
		last := (*seen)[len(*seen)-1]
		if last.path != testCase.providerPath || last.method != testCase.method {
			t.Fatalf("%s %s reached the provider as %s %s, want %s",
				testCase.method, testCase.facadePath, last.method, last.path, testCase.providerPath)
		}
	}

	// A path the facade does not mount is a 404 here rather than a hop. The
	// provider's own routes are not this facade's surface.
	if response := request(t, handler, http.MethodGet,
		"/api/v2/inventory/descriptor"); response.Code != http.StatusNotFound {
		t.Fatalf("an unmounted path answered %d", response.Code)
	}
	if len(*seen) != len(cases) {
		t.Fatalf("the provider saw %d requests for %d routes", len(*seen), len(cases))
	}
}

// Reading capacity and starting work are different grants, and the two methods
// that share the invocation path do not share one either.
func TestReadingIsNotInvoking(t *testing.T) {
	seen, cfg := recordingProvider(t)

	reader := route(t, cfg, inventory.ReadPermission)
	if response := request(t, reader, http.MethodGet,
		"/api/v2/inventory/slots/7"); response.Code != http.StatusOK {
		t.Fatalf("a reader could not see capacity: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, reader, http.MethodGet,
		"/api/v2/inventory/invocations/7/inventory/get_stats/abc"); response.Code != http.StatusOK {
		t.Fatalf("a reader could not poll: %d", response.Code)
	}
	// The cancel is on the SAME path as the poll and takes the other grant.
	if response := request(t, reader, http.MethodDelete,
		"/api/v2/inventory/invocations/7/inventory/get_stats/abc"); response.Code != http.StatusForbidden {
		t.Fatalf("a reader cancelled an invocation: %d", response.Code)
	}
	before := len(*seen)

	invoker := route(t, cfg, inventory.InvokePermission)
	if response := request(t, invoker, http.MethodGet,
		"/api/v2/inventory/slots/7"); response.Code != http.StatusForbidden {
		t.Fatalf("the invoke grant alone read capacity: %d", response.Code)
	}
	if response := request(t, invoker, http.MethodDelete,
		"/api/v2/inventory/invocations/7/inventory/get_stats/abc"); response.Code != http.StatusOK {
		t.Fatalf("the invoke grant could not cancel: %d", response.Code)
	}
	if len(*seen) != before+1 {
		t.Fatalf("%d refused requests still reached the provider", len(*seen)-before-1)
	}

	// No grant at all reaches nothing.
	none := route(t, cfg)
	for _, path := range []string{
		"/api/v2/inventory/slots/7", "/api/v2/inventory/invocations/7/inventory/get_stats/abc",
	} {
		if response := request(t, none, http.MethodGet, path); response.Code != http.StatusForbidden {
			t.Errorf("an ungranted caller reached %s: %d", path, response.Code)
		}
	}
}

// The three mounted paths carry the platform's API prefix.
//
// THIS IS A REGRESSION TEST FOR A LIVE DEFECT, not a style rule.
// production_router.go mounts these constants on the router ROOT, so the
// string IS the served path. They read `/inventory/...` until this commit,
// while DeepWiki's read `/api/v2/deepwiki/...`. The platform edge forwards
// `/api/v2` and nothing else, so the facade answered 200 to a request made
// directly against elitea-main's port inside the compose network and 404 to
// the identical request through traefik — measured on a running stack. Every
// test in this package passed throughout, because they all drove the composed
// handler directly and never asked where it was mounted.
//
// Compared against DeepWiki's constants rather than a literal, so a change to
// the platform prefix moves both providers or fails here.
func TestTheMountedPathsAreReachableThroughTheEdge(t *testing.T) {
	prefix := deepwiki.SlotsPath[:strings.Index(deepwiki.SlotsPath, "/deepwiki/")]
	if prefix == "" {
		t.Fatal("could not read the API prefix out of DeepWiki's slots path")
	}
	for name, path := range map[string]string{
		"SlotsPath":      inventory.SlotsPath,
		"InvokePath":     inventory.InvokePath,
		"InvocationPath": inventory.InvocationPath,
	} {
		if !strings.HasPrefix(path, prefix+"/inventory/") {
			t.Errorf("%s is %q; production_router.go mounts it on the router root, "+
				"so without the %q prefix the edge never forwards it and the route "+
				"is reachable only from inside the pod", name, path, prefix)
		}
	}
	// The two facades must not collide either: one provider's path must never
	// match another's pattern.
	if strings.HasPrefix(inventory.SlotsPath, prefix+"/deepwiki/") {
		t.Error("the Inventory facade is mounted under DeepWiki's path space")
	}
}

// The permissions are Inventory's own, not DeepWiki's. Migration 0108 seeds
// exactly these two strings, and a facade that resolved against another
// provider's grants would let one application's viewers drive the other.
func TestThePermissionsAreInventorysOwn(t *testing.T) {
	if inventory.ReadPermission != "models.applications.inventory.read" ||
		inventory.InvokePermission != "models.applications.inventory.invoke" {
		t.Fatalf("%q / %q", inventory.ReadPermission, inventory.InvokePermission)
	}
	if inventory.Mode != "default" {
		t.Fatalf("mode %q: migration 0108 seeds the default mode", inventory.Mode)
	}
}

// ---------------------------------------------------------------------------
// what crosses the hop
// ---------------------------------------------------------------------------

// The identity is this service's, signed here, and the client cannot
// contribute to it — even by sending the headers itself.
func TestTheProviderReceivesASignedIdentityTheClientCannotInfluence(t *testing.T) {
	seen, cfg := recordingProvider(t)
	handler := route(t, cfg, inventory.ReadPermission)

	r := httptest.NewRequest(http.MethodGet, "/api/v2/inventory/slots/7", nil)
	r.Header.Set("X-Auth-Type", "user")
	r.Header.Set("X-Auth-ID", "11")
	r.Header.Set(llmproxy.HeaderProjectID, "999")
	r.Header.Set(llmproxy.HeaderUserID, "1")
	r.Header.Set(llmproxy.HeaderSignature, "sha256=deadbeef")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}

	arrived := (*seen)[0].header
	if got := arrived.Get(llmproxy.HeaderProjectID); got != "7" {
		t.Errorf("project id %q — the client's 999 was not replaced by the path's 7", got)
	}
	if got := arrived.Get(llmproxy.HeaderUserID); got != "11" {
		t.Errorf("user id %q — the client's spoofed 1 survived", got)
	}
	if got := arrived.Get(llmproxy.HeaderSignature); got == "sha256=deadbeef" || got == "" {
		t.Errorf("signature %q — the client's own value was forwarded, or none was signed", got)
	}
}

// The caller's own credentials stay on this side. The provider authenticates
// the hop, not the end user, and has no use for a platform bearer.
func TestTheCallersCredentialsDoNotReachTheProvider(t *testing.T) {
	seen, cfg := recordingProvider(t)
	handler := route(t, cfg, inventory.ReadPermission)

	r := httptest.NewRequest(http.MethodGet, "/api/v2/inventory/slots/7", nil)
	r.Header.Set("X-Auth-Type", "user")
	r.Header.Set("X-Auth-ID", "11")
	r.Header.Set("Authorization", "Bearer platform-token")
	r.Header.Set("Cookie", "elitea_session=secret")
	r.Header.Set("X-Secret", "legacy-shared-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	for _, header := range []string{"Authorization", "Cookie", "X-Secret"} {
		if got := (*seen)[0].header.Get(header); got != "" {
			t.Errorf("%s reached the provider as %q", header, got)
		}
	}
	if (*seen)[0].host == r.Host {
		t.Errorf("the caller's Host (%q) was forwarded to the provider", r.Host)
	}
}

// ---------------------------------------------------------------------------
// composition
// ---------------------------------------------------------------------------

// Every way the facade can be asked for something it cannot authenticate is a
// REFUSAL, not a mount. A route that composed without a principal validator
// would serve every request perfectly well and check nothing, which is the
// failure this repository keeps finding.
func TestAFacadeThatCannotAuthenticateIsNotComposed(t *testing.T) {
	_, cfg := provider(t, http.StatusOK)
	full := authConfig()

	cases := []struct {
		name        string
		auth        apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		{"no principal validator",
			apimw.AuthConfig{ForwardedIdentityVerifier: full.ForwardedIdentityVerifier}, permissions()},
		{"no way to authenticate the caller",
			apimw.AuthConfig{PrincipalValidator: full.PrincipalValidator}, permissions()},
		{"no permission resolver", full, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			built, err := inventory.NewRoute(cfg, testCase.auth, testCase.permissions, nil, silent())
			if err == nil || built != nil {
				t.Fatalf("composed anyway: %v %v", built, err)
			}
			if err != inventory.ErrInvalidRoute {
				t.Fatalf("%v, want ErrInvalidRoute", err)
			}
		})
	}
}

// A transport the facade cannot build is a boot failure, not a mount that
// answers 502 per request. The provider terminates mTLS and refuses everything
// without a client certificate, so a facade that came up regardless would be
// three routes reporting an upstream problem while the flag says the feature
// is on.
func TestAnUnusableTransportRefusesToCompose(t *testing.T) {
	_, working := provider(t, http.StatusOK)

	cases := map[string]func(facade.Config) facade.Config{
		"a plain-HTTP base URL": func(cfg facade.Config) facade.Config {
			cfg.BaseURL = "http://elitea-inventory:8080"
			return cfg
		},
		"no base URL": func(cfg facade.Config) facade.Config {
			cfg.BaseURL = ""
			return cfg
		},
		"a client certificate that is not there": func(cfg facade.Config) facade.Config {
			cfg.ClientCertFile = cfg.ClientCertFile + ".missing"
			return cfg
		},
		"a CA that is not there": func(cfg facade.Config) facade.Config {
			cfg.CAFile = cfg.CAFile + ".missing"
			return cfg
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			built, err := inventory.NewRoute(break_(working), authConfig(),
				permissions(inventory.ReadPermission), nil, silent())
			if err == nil || built != nil {
				t.Fatalf("composed anyway: %v %v", built, err)
			}
		})
	}
}

// A mount that half-happened answers a readable 503 rather than taking the
// process down. Nil is what router.go holds for a deployment with the facade
// off, and every one of the three paths goes through this method.
func TestAnUnmountedFacadeAnswers503RatherThanPanicking(t *testing.T) {
	var absent *inventory.Route
	for _, path := range []string{
		inventory.SlotsPath, inventory.InvokePath, inventory.InvocationPath,
	} {
		response := httptest.NewRecorder()
		absent.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+path, nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "not enabled") {
			t.Fatalf("%s: %q", path, response.Body.String())
		}
	}
	// A Route value with no handler is the same case: composed, and carrying
	// nothing to serve.
	response := httptest.NewRecorder()
	(&inventory.Route{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/inventory/slots/7", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("an empty Route answered %d", response.Code)
	}
}

// The env names are spelled out and they are Inventory's. Both the chart and
// the env-drift gate are searched by the literal string, so a name built from
// a prefix would be invisible to them.
func TestTheEnvNamesAreTheOnesTheChartSets(t *testing.T) {
	want := facade.EnvNames{
		Enabled:        "ELITEA_INVENTORY_ENABLED",
		BaseURL:        "ELITEA_INVENTORY_BASE_URL",
		ClientCertFile: "ELITEA_INVENTORY_CLIENT_CERT_FILE",
		ClientKeyFile:  "ELITEA_INVENTORY_CLIENT_KEY_FILE",
		CAFile:         "ELITEA_INVENTORY_CA_FILE",
		ServerName:     "ELITEA_INVENTORY_SERVER_NAME",
		IdentitySecret: "ELITEA_INVENTORY_IDENTITY_SECRET",
		Timeout:        "ELITEA_INVENTORY_TIMEOUT_SECONDS",
	}
	if inventory.EnvNames != want {
		t.Fatalf("%+v", inventory.EnvNames)
	}
	// The reader they drive: a deployment that sets the flag and nothing else
	// must fail rather than mount a facade with no peer to reach.
	if _, err := facade.ConfigFromEnv(inventory.EnvNames, func(key string) (string, bool) {
		return map[string]string{"ELITEA_INVENTORY_ENABLED": "true"}[key], key == "ELITEA_INVENTORY_ENABLED"
	}); err == nil {
		t.Fatal("an enabled facade with no base URL was read as complete")
	}
	// Off is off: nothing else is required, and nothing is composed.
	cfg, err := facade.ConfigFromEnv(inventory.EnvNames, func(string) (string, bool) { return "", false })
	if err != nil || cfg.Enabled {
		t.Fatalf("%v %+v", err, cfg)
	}
}

// ---------------------------------------------------------------------------
// admission
// ---------------------------------------------------------------------------

// The admission gate is asked on the INVOKE and on nothing else. Refusing a
// poll would leave a run accepted before a revocation unobservable, and
// refusing a cancel would put the only way to stop it behind the control that
// just turned the provider off.
func TestAdmissionGatesTheInvokeAndNotTheReads(t *testing.T) {
	seen, cfg := recordingProvider(t)
	var asked int
	cfg.Admission = func(*http.Request) (bool, string) {
		asked++
		return false, "provider_admission_revoked"
	}
	handler := route(t, cfg, inventory.ReadPermission, inventory.InvokePermission)

	for _, testCase := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/inventory/slots/7"},
		{http.MethodGet, "/api/v2/inventory/invocations/7/inventory/get_stats/abc"},
		{http.MethodDelete, "/api/v2/inventory/invocations/7/inventory/get_stats/abc"},
	} {
		if response := request(t, handler, testCase.method, testCase.path); response.Code != http.StatusOK {
			t.Errorf("%s %s was refused by the admission gate: %d",
				testCase.method, testCase.path, response.Code)
		}
	}
	reached := len(*seen)

	body := strings.NewReader(`{"parameters":{}}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v2/inventory/tools/7/inventory/get_stats/invoke", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Auth-Type", "user")
	r.Header.Set("X-Auth-ID", "11")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("a revoked provider was invoked: %d %s", response.Code, response.Body.String())
	}
	if asked != 1 {
		t.Fatalf("the gate was asked %d times; it belongs on the invoke alone", asked)
	}
	if len(*seen) != reached {
		t.Fatal("a refused invoke still reached the provider")
	}
}

// The transport settings an operator writes are the ones the facade reads,
// and a value that cannot be parsed is a BOOT failure rather than a default.
// An operator who mistyped the timeout must learn at start, not at the first
// slow ingestion.
func TestTheTransportSettingsAreReadAsWritten(t *testing.T) {
	complete := map[string]string{
		"ELITEA_INVENTORY_ENABLED":          "true",
		"ELITEA_INVENTORY_BASE_URL":         "https://elitea-inventory:8080",
		"ELITEA_INVENTORY_CLIENT_CERT_FILE": "/certs/client.crt",
		"ELITEA_INVENTORY_CLIENT_KEY_FILE":  "/certs/client.key",
		"ELITEA_INVENTORY_CA_FILE":          "/certs/ca.crt",
		"ELITEA_INVENTORY_IDENTITY_SECRET":  "shared-with-the-provider",
	}
	read := func(overrides map[string]string) (facade.Config, error) {
		values := map[string]string{}
		for key, value := range complete {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		return facade.ConfigFromEnv(inventory.EnvNames, func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		})
	}

	cfg, err := read(map[string]string{"ELITEA_INVENTORY_TIMEOUT_SECONDS": "45"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 45*time.Second {
		t.Errorf("timeout %v, want the 45 seconds the operator wrote", cfg.Timeout)
	}
	if cfg.BaseURL != complete["ELITEA_INVENTORY_BASE_URL"] ||
		cfg.IdentitySecret != complete["ELITEA_INVENTORY_IDENTITY_SECRET"] {
		t.Errorf("%+v", cfg)
	}

	for name, overrides := range map[string]map[string]string{
		"a timeout that is not a number": {"ELITEA_INVENTORY_TIMEOUT_SECONDS": "soon"},
		"a timeout of zero":              {"ELITEA_INVENTORY_TIMEOUT_SECONDS": "0"},
		"a flag that is neither":         {"ELITEA_INVENTORY_ENABLED": "maybe"},
		"no identity secret":             {"ELITEA_INVENTORY_IDENTITY_SECRET": ""},
		"no client certificate":          {"ELITEA_INVENTORY_CLIENT_CERT_FILE": ""},
	} {
		if _, err := read(overrides); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

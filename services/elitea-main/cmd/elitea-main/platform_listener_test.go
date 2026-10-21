package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// platformStripFloor is the identity floor the retired platform-edge's
// strip-caller-auth-context middleware deleted, which
// tests/deployedge/edge_platform_identity_test.go held it to. The Go
// listener inherits the same floor: shrinking platformStrippedHeaders below it
// fails here.
var platformStripFloor = []string{
	"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Reference",
	"X-Auth-Signature", "X-Auth-Avatar", "X-Auth-Avatar-State",
	"X-Auth-Session-Id", "X-Auth-Session-Name", "X-Auth-Session-Endpoint",
	"X-Elitea-Identity-Signature", "X-Elitea-Project-Id", "X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
}

func TestPlatformListenerStripsCallerIdentityHeaders(t *testing.T) {
	var seen http.Header
	handler := platformListenerHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/1", nil)
	for _, name := range platformStripFloor {
		request.Header.Set(name, "caller-chosen")
	}
	request.Header.Set("X-Elitea-Execution-Id", "42")
	request.Header.Set("Authorization", "Bearer worker-token")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	for _, name := range platformStripFloor {
		if value := seen.Get(name); value != "" {
			t.Errorf("%s reached the handler as %q", name, value)
		}
	}
	// The worker tags model calls with the execution that made them; the
	// retired edge let it through and so does this listener.
	if seen.Get("X-Elitea-Execution-Id") != "42" {
		t.Errorf("X-Elitea-Execution-Id was stripped")
	}
	if seen.Get("Authorization") != "Bearer worker-token" {
		t.Errorf("the caller's own credential was stripped")
	}
}

func TestPlatformListenerRefusesTheInternalAddressSpace(t *testing.T) {
	reached := false
	handler := platformListenerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	for _, path := range []string{"/internal", "/internal/auth/main", "/internal/anything"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound || reached {
			t.Fatalf("%s: status %d, reached handler %v", path, recorder.Code, reached)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/internals-are-not-internal", nil))
	if !reached {
		t.Fatal("a path that only shares the prefix text was refused")
	}
}

func TestNoPlatformListenerWithoutConfiguration(t *testing.T) {
	for name, config := range map[string]runtimecomposition.Config{
		"runtime off":          {PlatformAddress: ":9446"},
		"runtime on, no addr":  {Enabled: true},
		"runtime off, no addr": {},
	} {
		server, err := newPlatformServer(config, http.NotFoundHandler())
		if err != nil || server != nil {
			t.Fatalf("%s: server=%v err=%v", name, server, err)
		}
	}
}

func TestPlatformListenerRefusesUnreadableMaterial(t *testing.T) {
	config := runtimecomposition.Config{Enabled: true, PlatformAddress: ":9446"}
	config.PlatformTLS.CertificateChainPath = "/nonexistent/content-server.crt"
	config.PlatformTLS.PrivateKeyPath = "/nonexistent/content-server.key"
	config.PlatformTLS.ClientCAPath = "/nonexistent/runtime-ca.crt"
	if _, err := newPlatformServer(config, http.NotFoundHandler()); err == nil {
		t.Fatal("a platform listener started without readable TLS material")
	}
}

// One failing listener stops the pair, and Shutdown drains both.
func TestApplicationServersStopTogether(t *testing.T) {
	public := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	// A platform server that cannot bind: no TLS material at all.
	platform := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	servers := applicationServers{public: public, platform: platform}
	done := make(chan error, 1)
	go func() { done <- servers.ListenAndServe() }()
	select {
	case err := <-done:
		// ListenAndServeTLS with no certificate and no TLSConfig fails to
		// open the (empty) certificate file name.
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe = %v, want the platform listener's TLS failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failing platform listener did not stop the pair")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

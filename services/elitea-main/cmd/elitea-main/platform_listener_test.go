package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The platform listener used to serve the public HTTP handler with the
// caller's identity headers stripped and /internal refused (the retired
// platform-edge's floor). It is gRPC-only now (#1359 D1): no route of the
// public handler — /internal included — and no caller-chosen header reaches
// anything on it. internal/transport/runtimegrpc/platform_server_test.go holds
// that gate over real TLS (HTTP/1.1 refused, per-service allowlist, health
// only to verified peers); this file holds the composition root to it.
func TestPlatformListenerRegistersOnlyAllowlistedServices(t *testing.T) {
	for _, service := range platformServices() {
		if service.Name == "" || !service.Clients.Configured() || service.Register == nil {
			t.Errorf("platform service %+v is not fully declared", service)
		}
	}
}

func TestNoPlatformListenerWithoutConfiguration(t *testing.T) {
	for name, config := range map[string]runtimecomposition.Config{
		"runtime off":          {PlatformAddress: ":9446"},
		"runtime on, no addr":  {Enabled: true},
		"runtime off, no addr": {},
	} {
		server, err := newPlatformServer(config, platformServices())
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
	if _, err := newPlatformServer(config, platformServices()); err == nil {
		t.Fatal("a platform listener started without readable TLS material")
	}
}

type failingPlatform struct{ err error }

func (p failingPlatform) ListenAndServe() error          { return p.err }
func (p failingPlatform) Shutdown(context.Context) error { return nil }

// One failing listener stops the pair, and Shutdown drains both.
func TestApplicationServersStopTogether(t *testing.T) {
	public := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	bindFailure := errors.New("listen for the platform listener: address in use")
	servers := applicationServers{public: public, platform: failingPlatform{err: bindFailure}}
	done := make(chan error, 1)
	go func() { done <- servers.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, bindFailure) {
			t.Fatalf("ListenAndServe = %v, want the platform listener's failure", err)
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

// A gRPC server's stop is a drain, which serveApplication recognises by
// http.ErrServerClosed; anything else stays a failure.
func TestPlatformServeResultIsADrainAfterStop(t *testing.T) {
	for _, stopped := range []error{nil, grpc.ErrServerStopped} {
		if err := platformServeResult(stopped); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("platformServeResult(%v) = %v, want http.ErrServerClosed", stopped, err)
		}
	}
	failure := errors.New("accept: too many open files")
	if err := platformServeResult(failure); !errors.Is(err, failure) {
		t.Errorf("platformServeResult kept %v as %v", failure, err)
	}
}

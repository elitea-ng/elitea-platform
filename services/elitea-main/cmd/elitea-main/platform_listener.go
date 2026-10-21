package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The platform listener (#1359) is elitea-main's ordinary product API — the
// SAME handler as the public cleartext server — served once more on an
// internal mutual-TLS port (ELITEA_RUNTIME_PLATFORM_ADDRESS, :9446). It is the
// worker's platform_origin, the internal MCP origin, Main's own self-call and
// the sub-application callback. It replaced the Traefik `platform-edge`, whose
// only job was to terminate TLS for those callers and forward cleartext to
// :8080.
//
// Authentication on the routes is unchanged (bearer, session cookie): the
// client certificate only admits the CONNECTION, and must chain to the runtime
// CA. Nothing here reads the certificate's identity.

// platformStrippedHeaders are the identity names deleted from every request
// on the platform listener, the floor the retired edge's
// strip-caller-auth-context middleware held. No caller of this listener may
// choose the identity elitea-main believes.
//
// X-Elitea-Execution-Id is deliberately ABSENT: the worker's model calls tag
// each request with the execution that made it (cost attribution in the LLM
// gateway), and elitea-main re-signs the value into the identity tuple.
var platformStrippedHeaders = []string{
	"X-Auth-Type",
	"X-Auth-ID",
	"X-Auth-User-ID",
	"X-Auth-Reference",
	"X-Auth-Signature",
	"X-Auth-Avatar",
	"X-Auth-Avatar-State",
	"X-Auth-Session-Id",
	"X-Auth-Session-Name",
	"X-Auth-Session-Endpoint",
	"X-Elitea-Identity-Signature",
	"X-Elitea-Project-Id",
	"X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
}

// platformListenerHandler wraps the public handler for the platform listener:
// it deletes the identity headers above and refuses elitea-main's internal
// address space (/internal), which the retired edge never routed either.
func platformListenerHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal" || strings.HasPrefix(r.URL.Path, "/internal/") {
			http.NotFound(w, r)
			return
		}
		for _, name := range platformStrippedHeaders {
			r.Header.Del(name)
		}
		next.ServeHTTP(w, r)
	})
}

// newPlatformServer builds the platform listener, or returns nil when the
// configuration opens none (the runtime is off, or no address is named).
func newPlatformServer(config runtimecomposition.Config, handler http.Handler) (*http.Server, error) {
	if !config.Enabled || config.PlatformAddress == "" {
		return nil, nil
	}
	tlsConfig, err := runtimegrpc.LoadPlatformServerTLSConfig(config.PlatformTLS)
	if err != nil {
		return nil, err
	}
	server := newHTTPServer(config.PlatformAddress, platformListenerHandler(handler))
	server.TLSConfig = tlsConfig
	return server, nil
}

// applicationServers runs the public server and, when present, the platform
// listener as ONE lifecycle component: serveApplication drains them together
// within one deadline, and either one failing stops both.
type applicationServers struct {
	public   *http.Server
	platform *http.Server
}

func (servers applicationServers) ListenAndServe() error {
	if servers.platform == nil {
		return servers.public.ListenAndServe()
	}
	results := make(chan error, 2)
	go func() { results <- servers.public.ListenAndServe() }()
	go func() { results <- servers.platform.ListenAndServeTLS("", "") }()
	// The first to stop decides; serveApplication then shuts both down. The
	// second result lands in the buffered channel and is dropped.
	return <-results
}

func (servers applicationServers) Shutdown(ctx context.Context) error {
	if servers.platform == nil {
		return servers.public.Shutdown(ctx)
	}
	errs := make(chan error, 2)
	go func() { errs <- servers.public.Shutdown(ctx) }()
	go func() { errs <- servers.platform.Shutdown(ctx) }()
	return errors.Join(<-errs, <-errs)
}

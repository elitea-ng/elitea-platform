// The compose browser edge, run for real (#1359 P2).
//
// The compose stacks ran a Traefik in front of elitea-main and the web SPA,
// and the gates in this directory read its dynamic files. That Traefik is
// gone: elitea-main IS the compose edge (internal/api/webedge, turned on by
// ELITEA_WEB_UPSTREAM). So the gates no longer parse a routing language. They
// start the real router with the web relay enabled, put a real HTTP upstream
// behind it that stands in for the web container, and send requests through a
// real listener. What a path reaches is then an observation, not a reading of
// a rule.
//
// "Reached elitea-main" is decided by two facts together: the web upstream did
// not see the request, and the answer carries the security headers elitea-main
// writes in front of every route (apimw.SecurityHeaders). The edge itself sits
// ahead of that middleware, so neither its redirect, its refusal nor anything
// it relays from the web container carries them.
package deployedge_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
)

// browserRemoteAddr is the peer the edge sees for a browser on a published
// port: anything but loopback. A probe inside the container is loopback.
const browserRemoteAddr = "192.0.2.10:52000"

// webBackendHeader marks every answer of the stand-in web container.
const webBackendHeader = "X-Test-Backend"

// composeEdge is one running edge: elitea-main's real router with the web
// relay on, and the stand-in web container behind it.
type composeEdge struct {
	edge *httptest.Server
	web  *httptest.Server

	mu      sync.Mutex
	webSeen []webRequest
}

type webRequest struct {
	path   string
	host   string
	header http.Header
}

// edgeAnswer is what one request through the edge produced.
type edgeAnswer struct {
	status      int
	header      http.Header
	body        string
	reachedWeb  bool
	reachedMain bool
	web         webRequest
}

// startComposeEdge builds the router from cfg with WebUpstream pointed at a
// fresh stand-in web container. remoteAddr, when set, replaces the peer
// address the router sees, which is how a test models a browser arriving on a
// published port rather than a loopback probe.
func startComposeEdge(t *testing.T, cfg api.RouterConfig, remoteAddr string) *composeEdge {
	t.Helper()
	edge := &composeEdge{}
	edge.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edge.mu.Lock()
		edge.webSeen = append(edge.webSeen, webRequest{path: r.URL.RequestURI(), host: r.Host, header: r.Header.Clone()})
		edge.mu.Unlock()
		w.Header().Set(webBackendHeader, "web")
		_, _ = io.WriteString(w, "web:"+r.URL.Path)
	}))
	t.Cleanup(edge.web.Close)

	upstream, err := url.Parse(edge.web.URL)
	if err != nil {
		t.Fatalf("parse the web upstream URL: %v", err)
	}
	cfg.WebUpstream = upstream
	router := api.NewRouter(cfg)
	edge.edge = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remoteAddr != "" {
			r.RemoteAddr = remoteAddr
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(edge.edge.Close)
	return edge
}

// do sends one request through the edge without following redirects.
func (e *composeEdge) do(t *testing.T, method, requestURI string, header http.Header) edgeAnswer {
	t.Helper()
	e.mu.Lock()
	before := len(e.webSeen)
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, e.edge.URL+requestURI, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, requestURI, err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s through the edge: %v", method, requestURI, err)
	}
	// Status and headers are what the gates read. A streaming route can keep
	// its body open, so the body is read only up to a bound.
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()

	answer := edgeAnswer{status: response.StatusCode, header: response.Header, body: string(body)}
	e.mu.Lock()
	if len(e.webSeen) > before {
		answer.reachedWeb = true
		answer.web = e.webSeen[len(e.webSeen)-1]
	}
	e.mu.Unlock()
	answer.reachedMain = !answer.reachedWeb && response.Header.Get("X-Frame-Options") == "DENY" &&
		response.Header.Get(webBackendHeader) == ""
	return answer
}

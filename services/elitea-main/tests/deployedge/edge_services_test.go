// This file gates WHICH PROCESS answers each browser path at the compose edge
// (#379, carried onto elitea-main's web relay by #1359 P2).
//
// The Traefik gate it replaces resolved the SERVICE each router named, because
// a router whose service did not resolve was dropped and its traffic fell to
// whichever router matched next: the caller got an answer from the wrong
// process. The edge is now Go, so the same failure is a path that reaches the
// wrong side of the relay. This file sends the paths through the real router
// and asserts the side each one reaches, and it checks that every browser-
// facing compose stack actually turns the relay on and publishes elitea-main.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// compose files this file reads live in deploy/, outside this module.
package deployedge_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webedge"
)

// webPaths are browser paths the web container serves: the SPA under its
// /app/ base, its hashed assets and runtime config, its project-switch deep
// link, and the embedded docs SPA (apps/elitea-web/nginx/spa.conf).
var webPaths = []string{
	"/app",
	"/app/",
	"/app/index.html",
	"/app/config.js",
	"/app/assets/index-abc123.js",
	"/app/chat",
	"/app/7/chat",
	"/app/7/assets/index-abc123.js",
	"/app/auth-callback",
	"/app/mcp-auth-callback",
	"/app/mcps",
	// The MCP carve-out is numeric project ids and the exact `mcp` segment
	// only; these are SPA paths.
	"/app/7/mcps",
	"/app/7/mcp-settings",
	"/app/abc/mcp",
	"/docs",
	"/docs/",
	"/docs/assets/index-abc123.js",
	"/docs/admin/authentication",
}

// mainPathsUnderWeb are elitea-main routes inside the SPA's namespace. The
// relay must leave each of them to elitea-main.
var mainPathsUnderWeb = []string{
	"/app/7/mcp",
	"/app/7/mcp/",
	"/app/7/mcp/sse",
	"/app/123/mcp/messages/abc",
	"/app/application_icon/1",
	"/app/application_tool_icon/1/2",
}

func TestComposeEdgeSendsEachPathToItsProcess(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)

	for _, path := range webPaths {
		answer := edge.do(t, http.MethodGet, path, nil)
		if !answer.reachedWeb {
			t.Errorf("GET %s did not reach the web container (status %d); the SPA would answer from elitea-main", path, answer.status)
			continue
		}
		if answer.web.path != path {
			t.Errorf("GET %s reached the web container as %s; the relay must not rewrite the path", path, answer.web.path)
		}
		if answer.status != http.StatusOK || answer.body != "web:"+path {
			t.Errorf("GET %s: the relay answered %d %q, not the web container's own answer", path, answer.status, answer.body)
		}
	}
	for _, path := range mainPathsUnderWeb {
		answer := edge.do(t, http.MethodGet, path, nil)
		if !answer.reachedMain {
			t.Errorf("GET %s did not reach elitea-main (web=%v, status %d); an MCP client or a tool icon would get the SPA", path, answer.reachedWeb, answer.status)
		}
	}
}

// TestComposeEdgeRelaysTheBrowserOrigin holds the two things the SPA's own
// redirects and links depend on: the query string survives, and the Host the
// browser used reaches the web container (Traefik's passHostHeader).
func TestComposeEdgeRelaysTheBrowserOrigin(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	answer := edge.do(t, http.MethodGet, "/app/chat?conversation=4&x=y", nil)
	if !answer.reachedWeb {
		t.Fatalf("GET /app/chat did not reach the web container (status %d)", answer.status)
	}
	if answer.web.path != "/app/chat?conversation=4&x=y" {
		t.Errorf("the web container saw %s; the query string must survive the relay", answer.web.path)
	}
	edgeHost := strings.TrimPrefix(edge.edge.URL, "http://")
	if answer.web.host != edgeHost {
		t.Errorf("the web container saw Host %q, want the browser's %q", answer.web.host, edgeHost)
	}
	if got := answer.web.header.Get("X-Forwarded-Host"); got != edgeHost {
		t.Errorf("X-Forwarded-Host = %q, want %q", got, edgeHost)
	}
}

// TestComposeEdgeIsOffWithoutAnUpstream holds the cluster posture: with
// WebUpstream unset, elitea-main relays nothing and redirects nothing, because
// the Gateway API HTTPRoute does that routing there.
func TestComposeEdgeIsOffWithoutAnUpstream(t *testing.T) {
	router := api.NewRouter(browserFacingRouterConfig())
	server := startPlainServer(t, router)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/", "/app/", "/docs/"} {
		response, err := client.Get(server + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with no web upstream answered %d, want elitea-main's own 404", path, response.StatusCode)
		}
	}
}

func TestParseUpstreamAcceptsOnlyAnOrigin(t *testing.T) {
	if upstream, err := webedge.ParseUpstream(""); upstream != nil || err != nil {
		t.Errorf("an empty value must leave the edge off, got %v, %v", upstream, err)
	}
	for _, good := range []string{"http://elitea-web:80", "http://elitea-web", "https://web.internal:8443/"} {
		if _, err := webedge.ParseUpstream(good); err != nil {
			t.Errorf("ParseUpstream(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"elitea-web:80", "ftp://web", "http://", "http://web/app", "http://web?x=1", "http://u:p@web"} {
		if _, err := webedge.ParseUpstream(bad); err == nil {
			t.Errorf("ParseUpstream(%q) accepted a value that is not a bare origin", bad)
		}
	}
}

// browserComposeStacks names the compose files whose stack serves a browser:
// every file under deploy/ that declares the web container. The floor below
// keeps a discovery that silently finds fewer of them from passing.
var browserComposeStackFloor = []string{
	"deploy/docker-compose.yml",
	"deploy/docker-compose.e2e-standalone.yml",
	"deploy/docker-compose.standalone-full.yml",
}

var webServiceNames = []string{"elitea-web"}

// TestBrowserComposeStacksTurnTheEdgeOn is the compose half of the service
// resolution gate: a stack whose elitea-main names no upstream, or one that
// does not exist in the same file, serves the API and a 404 for /app.
func TestBrowserComposeStacksTurnTheEdgeOn(t *testing.T) {
	root := repoRoot(t)
	files := composeFilesUnderDeploy(t, root)

	seen := map[string]bool{}
	for _, relative := range sortedKeys(files) {
		file := files[relative]
		webService := ""
		for _, name := range webServiceNames {
			if _, ok := file.Services[name]; ok {
				webService = name
			}
		}
		if webService == "" {
			continue
		}
		seen[relative] = true
		main, ok := file.Services["elitea-main"]
		if !ok {
			t.Errorf("%s declares %s and no elitea-main, so nothing serves it to a browser", relative, webService)
			continue
		}
		raw := main.environment()[webedge.UpstreamEnv]
		upstream, err := webedge.ParseUpstream(raw)
		if err != nil || upstream == nil {
			t.Errorf("%s: elitea-main %s = %q (%v). Without it this stack answers 404 for /app and /docs", relative, webedge.UpstreamEnv, raw, err)
			continue
		}
		if upstream.Hostname() != webService {
			t.Errorf("%s: %s names host %q, but the web container in this file is %q", relative, webedge.UpstreamEnv, upstream.Hostname(), webService)
		}
		if port := upstream.Port(); port != "" && port != "80" {
			t.Errorf("%s: %s names port %s; the web container's nginx listens on 80 (apps/elitea-web/nginx/spa.conf)", relative, webedge.UpstreamEnv, port)
		}
		if !publishesContainerPort(main.shortPorts(), "8080") {
			t.Errorf("%s: elitea-main publishes no port to its :8080 listener (ports %v), so the browser has no origin", relative, main.shortPorts())
		}
		if len(file.Services[webService].shortPorts()) > 0 {
			t.Errorf("%s: %s publishes a port. The browser reaches it through elitea-main; a second origin splits cookies and CORS", relative, webService)
		}
	}
	for _, floor := range browserComposeStackFloor {
		if !seen[floor] {
			t.Errorf("%s was not found as a browser-facing stack, so this gate read an incomplete tree", floor)
		}
	}
}

// publishesContainerPort reports whether a short-syntax mapping targets the
// container port.
func publishesContainerPort(ports []string, container string) bool {
	for _, mapping := range ports {
		mapping = strings.TrimSuffix(strings.TrimSuffix(mapping, "/tcp"), "/udp")
		if index := strings.LastIndex(mapping, ":"); index >= 0 && mapping[index+1:] == container {
			return true
		}
	}
	return false
}

// startPlainServer serves handler on a real listener and returns its URL.
func startPlainServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

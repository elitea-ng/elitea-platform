// This file gates the removal of edge authentication (#1359). It replaces the
// #378 gate, which held every edge forwardAuth to an address elitea-main
// registered.
//
// elitea-main registered exactly one such address, /internal/auth/main. Its
// only product was the X-Auth-* identity projection, and that plane is gone:
// elitea-main no longer registers the path, no longer reads the headers, and
// reads the Form session cookie itself. So the rule this gate applies is now
// one rule:
//
//	no edge configuration this repository ships may call an authenticator.
//
// A forwardAuth middleware calls one HTTP address before it admits a request
// and returns that address's non-2xx answer to the caller. Pointed at
// /internal/auth/main it would answer 404 for every request, so a router that
// names one fails at the edge. Pointed anywhere else it would be a second
// authentication plane this repository does not review.
//
// Unlike the #378 gate, a DEFINITION that no router names is a defect here
// too: there is no address left for it to call, and leaving one in place
// invites the next router to name it.
//
// The in-process half lives in elitea-main: internal/api's
// TestRouterStripsClientIdentityHeaders proves the root strip is mounted, and
// TestInProcessStripCoversTheEdgeFloor below holds its list to the edges'.
//
// The compose Traefik is gone too (#1359 P2): elitea-main is the compose edge,
// so the compose half of this gate asks elitea-main directly.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// removedAuthPath is the edge-auth address elitea-main used to register.
const removedAuthPath = "/internal/auth/main"

// TestNoEdgeServesTheRemovedAuthenticator replaces the parsed-Traefik half of
// this gate. The compose edge is elitea-main now, so the question "does a
// loaded edge call an authenticator" becomes "does elitea-main, AS the edge,
// still answer the address one would call". It must not: a 2xx there would be
// an authentication plane nobody reviews, and the address existing at all
// invites the next edge to name it. TestNoTraefikEdgeShips keeps a forwardAuth
// edge from coming back in a compose file, and the walk below covers the
// chart and the Gateway API manifests.
func TestNoEdgeServesTheRemovedAuthenticator(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		answer := edge.do(t, method, removedAuthPath, http.Header{"Cookie": {"elitea_session=forged"}})
		if answer.reachedWeb {
			t.Fatalf("%s %s reached the web container; the path routing changed under this gate", method, removedAuthPath)
		}
		if answer.status != http.StatusNotFound && answer.status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d through the compose edge. Edge authentication was removed in #1359; "+
				"elitea-main must not serve that address", method, removedAuthPath, answer.status)
		}
	}
}

// TestNoDeployConfigurationCallsTheRemovedAuthPath covers what the parsed gate
// cannot: Helm templates (not YAML until rendered), the Gateway API manifests,
// and any file no configuration set declares yet.
func TestNoDeployConfigurationCallsTheRemovedAuthPath(t *testing.T) {
	root := repoRoot(t)
	deploy := filepath.Join(root, "deploy")

	scanned := 0
	sawSentinel := false
	err := filepath.WalkDir(deploy, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".yml", ".yaml", ".tpl":
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		relative, _ := filepath.Rel(root, path)
		if relative == sentinelDeployFile {
			sawSentinel = true
		}
		for number, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, removedAuthPath) || strings.HasPrefix(trimmed, "forwardAuth:") {
				t.Errorf("%s:%d: %q — edge authentication was removed in #1359", relative, number+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", deploy, err)
	}
	if scanned == 0 || !sawSentinel {
		t.Fatalf(
			"scanned %d files under deploy/ and %s seen=%v, so this "+
				"gate proved nothing. deploy/ moved; update the walk.",
			scanned, sentinelDeployFile, sawSentinel,
		)
	}
}

// TestInProcessStripCoversTheEdgeFloor holds elitea-main's own strip list
// (apimw.ClientIdentityHeaders) to the floor the browser edges were held to.
// In compose that list IS the edge strip; in a cluster the HTTPRoute strips as
// well. A name on the floor that elitea-main keeps is one a deployment without
// a stripping edge would trust.
//
// X-Elitea-Execution-Id is the one exemption, for the reason
// apimw.StripClientIdentityHeaders records: it is legitimate client input on
// /llm, verified there, and stripping it would drop desktop cost attribution.
func TestInProcessStripCoversTheEdgeFloor(t *testing.T) {
	const executionHeader = "X-Elitea-Execution-Id"
	for _, name := range requiredStrippedHeaders {
		if strings.EqualFold(name, executionHeader) {
			if containsFold(apimw.ClientIdentityHeaders, name) {
				t.Errorf("apimw.ClientIdentityHeaders deletes %s, which the desktop and the worker send on /llm", name)
			}
			continue
		}
		if !containsFold(apimw.ClientIdentityHeaders, name) {
			t.Errorf("apimw.ClientIdentityHeaders does not delete %s, which every browser edge deletes", name)
		}
	}
}

// sentinelDeployFile is a template the walk must reach, so a walk that
// silently stops matching files cannot pass.
const sentinelDeployFile = "deploy/helm/elitea/templates/main/deployment.yaml"

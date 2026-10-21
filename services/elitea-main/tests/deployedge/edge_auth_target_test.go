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
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// removedAuthPath is the edge-auth address elitea-main used to register.
const removedAuthPath = "/internal/auth/main"

// edgeAuthMiddleware is one middleware definition, as far as this gate reads
// it.
type edgeAuthMiddleware struct {
	ForwardAuth *struct {
		Address string `yaml:"address"`
	} `yaml:"forwardAuth"`
}

// edgeWithEdgeAuth re-reads the edge YAML with the fields this gate needs.
type edgeWithEdgeAuth struct {
	HTTP struct {
		Routers map[string]struct {
			Middlewares []string `yaml:"middlewares"`
		} `yaml:"routers"`
		Middlewares map[string]edgeAuthMiddleware `yaml:"middlewares"`
	} `yaml:"http"`
}

func parseEdgeWithEdgeAuth(t *testing.T, path string) edgeWithEdgeAuth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed edgeWithEdgeAuth
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", path, err)
	}
	return parsed
}

// TestNoLoadedEdgeDefinesAForwardAuth is the gate over every configuration set
// a stack actually loads.
func TestNoLoadedEdgeDefinesAForwardAuth(t *testing.T) {
	root := repoRoot(t)

	routersChecked := 0
	for _, set := range configSets() {
		for _, path := range set.resolve(t, root) {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}
			parsed := parseEdgeWithEdgeAuth(t, path)
			routersChecked += len(parsed.HTTP.Routers)

			names := make([]string, 0, len(parsed.HTTP.Middlewares))
			for name := range parsed.HTTP.Middlewares {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				definition := parsed.HTTP.Middlewares[name]
				if definition.ForwardAuth == nil {
					continue
				}
				t.Errorf(
					"set %q: %s defines forwardAuth middleware %q (address %q).\n"+
						"Edge authentication was removed in #1359: elitea-main "+
						"authenticates every request itself, and %s no longer "+
						"exists, so the edge would return its 404 to every "+
						"caller. Delete the middleware and every reference to it.",
					set.name, relative, name, definition.ForwardAuth.Address, removedAuthPath,
				)
			}
		}
	}

	if routersChecked == 0 {
		t.Fatal(
			"no configuration set declares a router, so this gate proved " +
				"nothing. The edge changed shape, or a set stopped being declared.",
		)
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
// The edges are no longer what makes elitea-main safe; a name the edges delete
// and elitea-main keeps is one a deployment without those edges would trust.
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

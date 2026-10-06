// This file widens the edge gate to the SERVICE reference (#379).
//
// TestEveryRouterMiddlewareResolves in edge_middlewares_test.go resolves the
// MIDDLEWARE a router names. A router also names a service, and that reference
// fails in the same silent way: Traefik drops a router that names a service no
// loaded file defines. It logs the error and keeps serving, and the caller gets
// an answer from whichever router matches next on a path the configuration
// says goes to elitea-main. That is the exact failure of #338, through a
// different reference.
//
// (A second check here compared the published port in the hybrid edge's
// authority header with its Compose default. It retired with
// deploy/centry-hybrid, the only edge that rewrote the authority.)
//
// This gate needs no container and no network. It stays with the other edge
// gates in the No Binaries workflow, which carries no path filter.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// edgeWithServices re-reads the edge YAML with the fields this gate needs.
// dynamicConfig in edge_middlewares_test.go models middleware CHAINS and stops
// there; each gate parses its own shape, so no gate changes what another one
// reads.
type edgeWithServices struct {
	HTTP struct {
		Routers map[string]struct {
			Service string `yaml:"service"`
		} `yaml:"routers"`
		Services    map[string]struct{} `yaml:"services"`
		Middlewares map[string]struct {
			Headers struct {
				CustomRequestHeaders map[string]string `yaml:"customRequestHeaders"`
			} `yaml:"headers"`
		} `yaml:"middlewares"`
	} `yaml:"http"`
}

func parseEdgeWithServices(t *testing.T, path string) edgeWithServices {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed edgeWithServices
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", path, err)
	}
	return parsed
}

// isInternalProviderReference reports a reference that Traefik itself provides,
// such as `noop@internal`. No file defines those names, so the gate accepts
// them. The test on the suffix runs BEFORE providerLocalName drops it.
func isInternalProviderReference(reference string) bool {
	return strings.HasSuffix(reference, "@internal")
}

// TestEveryRouterServiceResolves is the gate for the service reference.
func TestEveryRouterServiceResolves(t *testing.T) {
	root := repoRoot(t)
	sets := configSets()
	if len(sets) == 0 {
		t.Fatal("no configuration set is declared, so nothing is gated")
	}

	checked := 0
	for _, set := range sets {
		paths := set.resolve(t, root)

		defined := map[string]bool{}
		for _, name := range set.externalServices {
			defined[name] = true
		}
		type reference struct {
			router  string
			file    string
			service string
		}
		var references []reference

		for _, path := range paths {
			parsed := parseEdgeWithServices(t, path)
			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}
			for name := range parsed.HTTP.Services {
				defined[name] = true
			}
			for router, definition := range parsed.HTTP.Routers {
				// Traefik rejects an HTTP router that names no service. An
				// empty value is a typing mistake, not a default.
				if strings.TrimSpace(definition.Service) == "" {
					t.Errorf(
						"set %q: router %q in %s names no service.\n"+
							"Traefik needs one service for each HTTP router.\n"+
							"Set boundary: %s",
						set.name, router, relative, set.mountedBy,
					)
					continue
				}
				references = append(references, reference{
					router: router, file: relative, service: definition.Service,
				})
			}
		}

		for _, ref := range references {
			if isInternalProviderReference(ref.service) {
				checked++
				continue
			}
			checked++
			if defined[providerLocalName(ref.service)] {
				continue
			}
			known := make([]string, 0, len(defined))
			for name := range defined {
				known = append(known, name)
			}
			sort.Strings(known)
			t.Errorf(
				"set %q: router %q in %s names service %q, which no file in "+
					"this set defines.\n"+
					"Traefik does not fail the stack for this. It drops the "+
					"router and keeps serving, so the traffic falls to "+
					"whichever router matches next, and the caller gets an "+
					"answer from the wrong process.\n"+
					"Defined in this set: %s\n"+
					"Set boundary: %s",
				set.name, ref.router, ref.file, ref.service,
				strings.Join(known, ", "), set.mountedBy,
			)
		}
	}

	if checked == 0 {
		t.Fatal("no router service reference was inspected, so this gate proved nothing")
	}
}

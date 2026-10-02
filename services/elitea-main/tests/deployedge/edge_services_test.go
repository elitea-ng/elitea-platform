// This file widens the edge gate to the two references it did not cover
// (#379).
//
// TestEveryRouterMiddlewareResolves in edge_middlewares_test.go resolves the
// MIDDLEWARE a router names. A router names two more things, and each one fails
// in the same silent way:
//
//	1. the SERVICE. Traefik drops a router that names a service no loaded file
//	   defines. It logs the error and keeps serving. In deploy/centry-hybrid the
//	   dropped router is the one that selects Go, base.yml holds a
//	   PathPrefix("/") catch-all to pylon at priority 1, and the caller gets
//	   HTTP 200 from pylon on a path the configuration says goes to elitea-main.
//	   That is the exact failure of #338, through a different reference.
//	2. the published PORT in the authority header. The value in
//	   normalize-runtime-public-authority is a literal, because the Traefik file
//	   provider does not expand environment variables. It must equal the
//	   ELITEA_HYBRID_HTTPS_PORT default in the Compose file. Change the Compose
//	   file alone and the routers still load, but the authority header then
//	   names a port that nothing listens on.
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
	"regexp"
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
					"whichever router matches next. In deploy/centry-hybrid "+
					"that is the PathPrefix(\"/\") catch-all to pylon, and the "+
					"caller gets HTTP 200 from the wrong process.\n"+
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

// authorityMiddlewareName is the middleware that rewrites the request authority
// before the edge-auth call. It carries the published port as a literal.
const authorityMiddlewareName = "normalize-runtime-public-authority"

// hybridPortVariable is the Compose variable that selects the published port of
// the hybrid edge.
const hybridPortVariable = "ELITEA_HYBRID_HTTPS_PORT"

// hybridPortSources are the tracked files that state the default of
// hybridPortVariable. The Compose file is the deployment. README.md documents
// the same value as the public URL. traefik/middlewares.yml requires all of
// them to agree, and this gate is what enforces that requirement.
var hybridPortSources = []string{
	"deploy/centry-hybrid/docker-compose.yml",
	"deploy/centry-hybrid/README.md",
}

// hybridPortPattern matches the Compose default form `${NAME:-18443}`.
var hybridPortPattern = regexp.MustCompile(`\$\{` + hybridPortVariable + `:-([0-9]+)\}`)

// TestRuntimeAuthorityPortMatchesTheHybridComposeDefault is the gate for the
// hardcoded port.
func TestRuntimeAuthorityPortMatchesTheHybridComposeDefault(t *testing.T) {
	root := repoRoot(t)

	// 1. Read the literal out of the middleware definition. The gate looks in
	//    every configuration set, so it keeps working when the definition moves
	//    to another edge file. It FAILS when no set defines the middleware at
	//    all: "nothing found, therefore pass" is how a gate stops gating.
	authorityValue := ""
	authoritySource := ""
	for _, set := range configSets() {
		for _, path := range set.resolve(t, root) {
			parsed := parseEdgeWithServices(t, path)
			definition, present := parsed.HTTP.Middlewares[authorityMiddlewareName]
			if !present {
				continue
			}
			value := definition.Headers.CustomRequestHeaders["Host"]
			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}
			if authorityValue != "" && authorityValue != value {
				t.Fatalf(
					"%s and %s both define %q with a different Host value: %q and %q.\n"+
						"Two edges cannot rewrite the authority to two different "+
						"authorities. Make them equal, or give the second one its "+
						"own name.",
					authoritySource, relative, authorityMiddlewareName,
					authorityValue, value,
				)
			}
			authorityValue = value
			authoritySource = relative
		}
	}
	if authoritySource == "" {
		t.Fatalf(
			"no configuration set defines the %q middleware, so this gate "+
				"stopped gating. The middleware moved or it was renamed. "+
				"Update authorityMiddlewareName in this file, or update "+
				"configSets() in edge_middlewares_test.go.",
			authorityMiddlewareName,
		)
	}
	if authorityValue == "" {
		t.Fatalf(
			"%s defines %q with no Host request header.\n"+
				"That middleware exists to rewrite the request authority. "+
				"Without the header it rewrites nothing, and the edge-auth "+
				"call answers 403 on the worker's own authority.",
			authoritySource, authorityMiddlewareName,
		)
	}
	colon := strings.LastIndex(authorityValue, ":")
	if colon < 0 {
		t.Fatalf(
			"%s: %q rewrites the authority to %q, which carries no port.\n"+
				"The edge publishes ${%s}, so the authority must name that "+
				"port. TrustedProxyResolver compares the whole authority "+
				"against the configured public origin and answers 403 on a "+
				"mismatch.",
			authoritySource, authorityMiddlewareName, authorityValue,
			hybridPortVariable,
		)
	}
	authorityPort := authorityValue[colon+1:]

	// 2. Read the default out of each tracked source, and require one value.
	for _, relative := range hybridPortSources {
		absolute := filepath.Join(root, relative)
		raw, err := os.ReadFile(absolute)
		if err != nil {
			t.Fatalf(
				"read %s: %v.\nThe file moved and this gate stopped gating. "+
					"Update hybridPortSources in this file.",
				relative, err,
			)
		}
		matches := hybridPortPattern.FindAllStringSubmatch(string(raw), -1)
		if len(matches) == 0 {
			t.Fatalf(
				"%s states no ${%s:-<port>} default, so this gate stopped "+
					"gating. Update hybridPortSources in this file.",
				relative, hybridPortVariable,
			)
		}
		for _, match := range matches {
			if match[1] == authorityPort {
				continue
			}
			t.Errorf(
				"%s defaults %s to %s, but %s rewrites the authority to %q.\n"+
					"The Traefik file provider does not expand environment "+
					"variables, so the authority is a literal and the two "+
					"drift apart silently. The routers still load. The "+
					"edge-auth call then answers 403, because "+
					"TrustedProxyResolver compares the authority against the "+
					"configured public origin.\n"+
					"Change both together.",
				relative, hybridPortVariable, match[1],
				authoritySource, authorityValue,
			)
		}
	}
}

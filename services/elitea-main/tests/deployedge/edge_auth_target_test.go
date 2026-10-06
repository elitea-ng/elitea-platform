// This file gates the edge-auth target of the edge configuration (#378).
//
// An edgeAuth middleware calls one HTTP address before it admits a request.
// Traefik returns the answer of that address to the caller when the answer is
// not 2xx. An address that nothing registers therefore breaks every router that
// names the middleware, and it breaks them at the edge.
//
// The retired deploy/centry-hybrid edge pointed `go-main-auth` at
// http://elitea-main:8080/internal/auth/main. elitea-main registers
// that path in internal/api/production_router.go, inside the branch that
// composes production authentication. cmd/elitea-main/main.go enters that
// branch only when ELITEA_AUTH_CONFIG_FILE is set. So the rule this gate
// applies is one rule:
//
//	a router may name an edgeAuth middleware only when the Compose service
//	behind its address sets ELITEA_AUTH_CONFIG_FILE.
//
// The foundation stack broke that rule. It mounted a whole directory, so it
// loaded index-routes.yml as a side effect, and eight routers there name
// go-main-auth. Three of them carry no Host(`elitea-gateway`) guard and
// sit at priority 90, so a browser reached them and the notification API failed
// at the edge.
//
// A DEFINITION that no router names is not a defect here. Traefik never calls
// it. This gate follows the router, because that is what Traefik acts on.
//
// This gate needs no container and no network. It stays with the other edge
// gates in the No Binaries workflow, which carries no path filter.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
)

// authConfigVariable selects production authentication. main.go reads it, and
// production_router.go registers the edge-auth path only in that branch.
const authConfigVariable = "ELITEA_AUTH_CONFIG_FILE"

// edgeAuthMiddleware is one middleware definition, with the address this
// gate follows and the chain members that reach further definitions.
type edgeAuthMiddleware struct {
	EdgeAuth struct {
		Address string `yaml:"address"`
	} `yaml:"forwardAuth"`
	Chain struct {
		Middlewares []string `yaml:"middlewares"`
	} `yaml:"chain"`
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

// composeModel is the subset of a Compose file this gate reads.
type composeModel struct {
	Services map[string]struct {
		Environment yaml.Node `yaml:"environment"`
	} `yaml:"services"`
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

func parseCompose(t *testing.T, root, relative string) composeModel {
	t.Helper()
	absolute := filepath.Join(root, relative)
	raw, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatalf(
			"read %s: %v.\nThe Compose file moved and this gate stopped "+
				"gating. Update configSets() in edge_middlewares_test.go.",
			relative, err,
		)
	}
	var parsed composeModel
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as a Compose model: %v", relative, err)
	}
	if len(parsed.Services) == 0 {
		t.Fatalf(
			"%s declares no services, so this gate stopped gating. The file "+
				"changed shape. Update configSets() in edge_middlewares_test.go.",
			relative,
		)
	}
	return parsed
}

// environmentValue reads one variable out of a Compose `environment` block.
// Compose accepts a mapping and a `NAME=value` sequence, and this gate accepts
// both. The second return value reports that the name is present.
func environmentValue(node yaml.Node, name string) (string, bool) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == name {
				return node.Content[index+1].Value, true
			}
		}
	case yaml.SequenceNode:
		for _, entry := range node.Content {
			key, value, found := strings.Cut(entry.Value, "=")
			if found && key == name {
				return value, true
			}
		}
	}
	return "", false
}

// expandMiddlewares returns every middleware a router reaches, including the
// members of a chain middleware. A chain fails the same way a router does.
func expandMiddlewares(named []string, definitions map[string]edgeAuthMiddleware) []string {
	seen := map[string]bool{}
	var reached []string
	var walk func(references []string)
	walk = func(references []string) {
		for _, reference := range references {
			name := providerLocalName(reference)
			if seen[name] {
				continue
			}
			seen[name] = true
			reached = append(reached, name)
			definition, present := definitions[name]
			if !present {
				continue
			}
			walk(definition.Chain.Middlewares)
		}
	}
	walk(named)
	return reached
}

// TestEveryLoadedEdgeAuthTargetIsRegistered is the gate.
func TestEveryLoadedEdgeAuthTargetIsRegistered(t *testing.T) {
	root := repoRoot(t)

	checked := 0
	for _, set := range configSets() {
		paths := set.resolve(t, root)

		// Merge the set, because a router in one file names a middleware that
		// another file in the same set defines.
		definitions := map[string]edgeAuthMiddleware{}
		routerFile := map[string]string{}
		routerMiddlewares := map[string][]string{}
		for _, path := range paths {
			parsed := parseEdgeWithEdgeAuth(t, path)
			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}
			for name, definition := range parsed.HTTP.Middlewares {
				definitions[name] = definition
			}
			for router, definition := range parsed.HTTP.Routers {
				routerFile[router] = relative
				routerMiddlewares[router] = definition.Middlewares
			}
		}

		routers := make([]string, 0, len(routerMiddlewares))
		for router := range routerMiddlewares {
			routers = append(routers, router)
		}
		sort.Strings(routers)

		for _, router := range routers {
			for _, name := range expandMiddlewares(routerMiddlewares[router], definitions) {
				address := definitions[name].EdgeAuth.Address
				if strings.TrimSpace(address) == "" {
					continue
				}
				checked++
				assertEdgeAuthTarget(t, root, set, router, routerFile[router], name, address)
			}
		}
	}

	if checked == 0 {
		t.Fatal(
			"no router names an edgeAuth middleware in any configuration " +
				"set, so this gate proved nothing. The edge changed shape, or " +
				"a set stopped being declared.",
		)
	}
}

// assertEdgeAuthTarget applies the one rule to one reference.
func assertEdgeAuthTarget(
	t *testing.T,
	root string,
	set configSet,
	router, routerFile, middleware, address string,
) {
	t.Helper()

	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		t.Errorf(
			"set %q: middleware %q holds the edgeAuth address %q, which is "+
				"not a URL with a host: %v",
			set.name, middleware, address, err,
		)
		return
	}
	if parsed.Path != browserauth.MainEdgeAuthPath {
		t.Errorf(
			"set %q: middleware %q calls %q, and router %q in %s names it.\n"+
				"elitea-main registers exactly one internal edge-auth "+
				"path, %q (internal/api/browserauth). Every other path "+
				"answers 404, and Traefik returns that 404 to the caller.\n"+
				"Correct the address. Teach this gate the second path only "+
				"when elitea-main registers one.",
			set.name, middleware, address, router, routerFile,
			browserauth.MainEdgeAuthPath,
		)
		return
	}

	target := parsed.Hostname()
	if len(set.composeFiles) == 0 {
		t.Errorf(
			"set %q: router %q in %s names %q, which calls %q, but the set "+
				"declares no Compose file.\n"+
				"Nothing can then answer whether %s registers that path. "+
				"Declare composeFiles for this set in "+
				"edge_middlewares_test.go.\n"+
				"Set boundary: %s",
			set.name, router, routerFile, middleware, address, target,
			set.mountedBy,
		)
		return
	}

	defining := 0
	for _, relative := range set.composeFiles {
		compose := parseCompose(t, root, relative)
		service, present := compose.Services[target]
		if !present {
			continue
		}
		defining++
		value, configured := environmentValue(service.Environment, authConfigVariable)
		if configured && strings.TrimSpace(value) != "" {
			continue
		}
		t.Errorf(
			"set %q: router %q in %s names %q, which calls %q.\n"+
				"Service %q in %s does not set %s, so elitea-main composes no "+
				"production authentication and registers no edge-auth "+
				"path. cmd/elitea-main/main.go reads that variable, and "+
				"internal/api/production_router.go registers %q only inside "+
				"that branch.\n"+
				"Traefik returns the non-2xx answer of the authenticator to "+
				"the caller, so every route this router serves fails at the "+
				"edge.\n"+
				"Set %s for that service, or stop loading the router.\n"+
				"Set boundary: %s",
			set.name, router, routerFile, middleware, address,
			target, relative, authConfigVariable,
			browserauth.MainEdgeAuthPath, authConfigVariable, set.mountedBy,
		)
	}
	if defining == 0 {
		t.Errorf(
			"set %q: router %q in %s names %q, which calls %q, but no "+
				"declared Compose file defines a service named %q.\n"+
				"Declared: %s\n"+
				"The address names a container this stack does not start, so "+
				"the edge-auth call cannot be answered.",
			set.name, router, routerFile, middleware, address, target,
			strings.Join(set.composeFiles, ", "),
		)
	}
}

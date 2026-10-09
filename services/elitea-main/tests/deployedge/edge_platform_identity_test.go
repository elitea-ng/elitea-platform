// This file gates the identity-header handling of the worker's platform edge,
// deploy/runtime/platform-edge-dynamic.yml, and of its Helm transcription.
//
// elitea-main accepts an X-Auth-* identity only when it arrives with a valid
// X-Auth-Signature for the same request. The auth edge (runtime-auth) is the
// one party that produces that projection, so three properties hold for every
// router in this file that reaches elitea-main:
//
//  1. the strip middleware is the FIRST entry of the router's middleware
//     list, so it runs before any forwardAuth;
//  2. the strip middleware deletes every name in requiredStrippedHeaders,
//     except the documented exemptions below;
//  3. every forwardAuth middleware copies the full projection, including
//     X-Auth-Signature, onto the forwarded request.
//
// The catch-all router also excludes elitea-main's internal address space.
//
// The Helm chart renders the same configuration into a ConfigMap; the render
// test deploy/helm/tests/render-platform-edge-identity.sh asserts the same
// properties and the equality of the two copies.
package deployedge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const platformEdgeFile = "deploy/runtime/platform-edge-dynamic.yml"

const platformStripMiddlewareName = "strip-caller-auth-context"

// platformEdgeExemptHeaders are names in requiredStrippedHeaders that this
// edge must NOT delete, with the reason. The list is explicit on purpose.
//
// X-Elitea-Execution-Id: the runtime worker's model calls use platform_origin
// (this edge) as their base URL and tag each call with the execution it was
// made from, for per-execution cost attribution in the LLM gateway. The
// worker authenticates with its own bearer on that route; elitea-main
// re-signs the value into the identity tuple it forwards, and browser-facing
// edges still delete it.
var platformEdgeExemptHeaders = map[string]string{
	"X-Elitea-Execution-Id": "sent by the runtime worker on model calls through platform_origin",
}

// requiredAuthResponseHeaders is the projection every forwardAuth on this
// edge must copy onto the forwarded request.
var requiredAuthResponseHeaders = []string{
	"X-Auth-Type",
	"X-Auth-ID",
	"X-Auth-User-ID",
	"X-Auth-Signature",
}

type platformEdgeConfig struct {
	HTTP struct {
		Routers map[string]struct {
			Rule        string   `yaml:"rule"`
			Service     string   `yaml:"service"`
			Middlewares []string `yaml:"middlewares"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			Headers struct {
				CustomRequestHeaders map[string]string `yaml:"customRequestHeaders"`
			} `yaml:"headers"`
			ForwardAuth *struct {
				AuthResponseHeaders []string `yaml:"authResponseHeaders"`
			} `yaml:"forwardAuth"`
		} `yaml:"middlewares"`
	} `yaml:"http"`
}

func parsePlatformEdge(t *testing.T, path string) platformEdgeConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed platformEdgeConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", path, err)
	}
	return parsed
}

func TestPlatformEdgeIdentityHandling(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, platformEdgeFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s does not exist. The edge moved and this gate stopped gating. Update platformEdgeFile.", platformEdgeFile)
	}
	parsed := parsePlatformEdge(t, path)

	// (b) the strip middleware deletes the floor, minus the documented exemptions.
	strip, defined := parsed.HTTP.Middlewares[platformStripMiddlewareName]
	if !defined {
		t.Fatalf("%s defines no %q middleware", platformEdgeFile, platformStripMiddlewareName)
	}
	for name, value := range strip.Headers.CustomRequestHeaders {
		if value != "" {
			t.Errorf("%s: %q sets %s to %q; only an empty value deletes a header", platformEdgeFile, platformStripMiddlewareName, name, value)
		}
	}
	for _, name := range requiredStrippedHeaders {
		if _, exempt := platformEdgeExemptHeaders[name]; exempt {
			if _, present := strip.Headers.CustomRequestHeaders[name]; present {
				t.Errorf("%s: %q deletes %s, which the runtime worker sends through this edge (%s)",
					platformEdgeFile, platformStripMiddlewareName, name, platformEdgeExemptHeaders[name])
			}
			continue
		}
		present := false
		for have := range strip.Headers.CustomRequestHeaders {
			if strings.EqualFold(have, name) {
				present = true
			}
		}
		if !present {
			t.Errorf("%s: %q does not delete %s", platformEdgeFile, platformStripMiddlewareName, name)
		}
	}

	// (a) every router reaching elitea-main starts with the strip middleware.
	reaching := 0
	for name, router := range parsed.HTTP.Routers {
		if router.Service != protectedService {
			continue
		}
		reaching++
		if len(router.Middlewares) == 0 || providerLocalName(router.Middlewares[0]) != platformStripMiddlewareName {
			t.Errorf("%s: router %q reaches %q and its first middleware is not %q (middlewares: %v); the strip must run before any forwardAuth",
				platformEdgeFile, name, protectedService, platformStripMiddlewareName, router.Middlewares)
		}
	}
	if reaching == 0 {
		t.Errorf("%s declares no router with service %q, so this gate checked nothing", platformEdgeFile, protectedService)
	}

	// (c) every forwardAuth copies the full projection.
	auths := 0
	for name, middleware := range parsed.HTTP.Middlewares {
		if middleware.ForwardAuth == nil {
			continue
		}
		auths++
		for _, required := range requiredAuthResponseHeaders {
			if !containsFold(middleware.ForwardAuth.AuthResponseHeaders, required) {
				t.Errorf("%s: forwardAuth %q does not copy %s in authResponseHeaders", platformEdgeFile, name, required)
			}
		}
	}
	if auths == 0 {
		t.Errorf("%s declares no forwardAuth middleware, so this gate checked nothing", platformEdgeFile)
	}

	// (d) the catch-all does not route elitea-main's internal address space.
	catchAll, ok := parsed.HTTP.Routers["platform"]
	if !ok {
		t.Fatalf("%s declares no router named %q", platformEdgeFile, "platform")
	}
	if !strings.Contains(catchAll.Rule, "!PathPrefix(`/internal`)") {
		t.Errorf("%s: router %q rule %q does not exclude /internal; the worker never needs that address space through this edge",
			platformEdgeFile, "platform", catchAll.Rule)
	}
}

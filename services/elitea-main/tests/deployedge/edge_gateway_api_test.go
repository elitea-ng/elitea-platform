// The Gateway-API browser edge (#568; one route since #1359 P3).
//
// WHAT WAS MISSING.
//
// The cluster's edge was a Gateway API HTTPRoute that lived in the deployment
// repository alone, so no gate here could read it. PR #564 added four path
// prefixes to the compose edge; the cluster edge never got them, and project
// icons and author avatars stayed broken there for the whole 1.20.1 window.
// deploy/gateway-api/httproute.yaml became the reviewed source of that edge,
// with a rule per root-mounted Go route, and this file walked the Go router
// against it.
//
// WHAT CHANGED.
//
// elitea-main is now the browser edge in every deployment
// (internal/api/webedge): the chart sets ELITEA_WEB_UPSTREAM, and the
// HTTPRoute is ONE `/` rule to elitea-main. A per-route rule list is gone, so
// there is no second list to drift; the MCP carve-out, which Gateway API could
// only express with an implementation-specific RegularExpression, is decided in
// Go. edge_root_routes_test.go and edge_services_test.go classify every route
// through the real router with the relay on, and that classification now
// covers the cluster as well.
//
// WHAT THIS FILE HOLDS.
//
//  1. the manifest is that one rule, and nothing has crept back: no second
//     rule, no SPA backend, no RegularExpression, no narrower match;
//  2. the rule still removes the caller's identity headers (defence in depth:
//     elitea-main strips them itself), and never the LLM gateway's hop marker;
//  3. the chart's own HTTPRoute list obeys the same floor.
//
// WHAT IT STILL DOES NOT COVER.
//
// A copy of the manifest in a deployment repository. Nothing here reads the
// cluster. A post-deploy check that requests the root-mounted paths against the
// deployed host, discriminating on `vary: Origin` (elitea-main sets it), is the
// control for that gap (#568).
//
// RUN IT WITH -count=1, for the reason edge_middlewares_test.go gives: the
// files this gate reads live in deploy/, outside this module, so the test
// cache does not notice an edit to them.
package deployedge_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// gatewayAPIEdgeFiles are the checked-in Gateway-API manifests that serve a
// browser. The list exists so that a second cluster edge is added HERE and
// gains every check below on the day it is written.
var gatewayAPIEdgeFiles = []string{
	"deploy/gateway-api/httproute.yaml",
}

// chartValuesFile holds the chart's own HTTPRoute posture. The chart renders
// the same one-rule shape as the manifest above from a values list, and
// TestChartHTTPRouteStripsClientIdentity holds that list to the same floor.
const chartValuesFile = "deploy/helm/elitea/values.yaml"

// gatewayRoute is the subset of an HTTPRoute this gate reads.
type gatewayRoute struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Rules []gatewayRule `yaml:"rules"`
	} `yaml:"spec"`
}

type gatewayRule struct {
	Matches []struct {
		Path struct {
			Type  string `yaml:"type"`
			Value string `yaml:"value"`
		} `yaml:"path"`
		// The three match fields this gate does NOT read. A match that also
		// requires a header, a method or a query parameter forwards LESS than
		// its path says, so reading the path alone would report a route as
		// covered when it is not. The gate fails on them instead of ignoring
		// them.
		Headers     []map[string]string `yaml:"headers"`
		QueryParams []map[string]string `yaml:"queryParams"`
		Method      string              `yaml:"method"`
	} `yaml:"matches"`
	Filters []struct {
		Type                  string `yaml:"type"`
		RequestHeaderModifier struct {
			Remove []string `yaml:"remove"`
			Set    []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"set"`
			Add []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"add"`
		} `yaml:"requestHeaderModifier"`
	} `yaml:"filters"`
	BackendRefs []struct {
		Name string `yaml:"name"`
		Port int    `yaml:"port"`
	} `yaml:"backendRefs"`
}

// parseGatewayAPIEdge reads every HTTPRoute document of one manifest. It fails
// closed: a file that has moved, a file that holds no HTTPRoute, and a document
// this gate cannot read are all failures, because each one turns the gate
// silently into a no-op.
func parseGatewayAPIEdge(t *testing.T, root, relative string) []gatewayRoute {
	t.Helper()
	absolute := filepath.Join(root, relative)
	raw, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatalf(
			"%s does not open: %v.\n"+
				"The Gateway-API edge moved and this gate stopped gating. Update gatewayAPIEdgeFiles in this file.",
			relative, err,
		)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var routes []gatewayRoute
	for {
		var document gatewayRoute
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s as Gateway API manifests: %v", relative, err)
		}
		if document.Kind == "" {
			// An empty document, which a leading `---` produces.
			continue
		}
		if document.Kind != "HTTPRoute" {
			t.Fatalf(
				"%s holds a %s document. This gate reads HTTPRoute only, and it does not skip what it "+
					"cannot read: an unread document is an edge rule nothing checks.",
				relative, document.Kind,
			)
		}
		if !strings.HasPrefix(document.APIVersion, "gateway.networking.k8s.io/") {
			t.Fatalf(
				"%s: HTTPRoute %q declares apiVersion %q. This gate reads the gateway.networking.k8s.io group.",
				relative, document.Metadata.Name, document.APIVersion,
			)
		}
		routes = append(routes, document)
	}
	if len(routes) == 0 {
		t.Fatalf(
			"%s holds no HTTPRoute, so this gate read nothing.\n"+
				"An empty edge file passes every check below while the cluster keeps serving the old rules.",
			relative,
		)
	}
	return routes
}

// mainBackendRules returns the rules that send traffic to elitea-main.
func mainBackendRules(routes []gatewayRoute) []gatewayRule {
	var reaching []gatewayRule
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Name == protectedService {
					reaching = append(reaching, rule)
					break
				}
			}
		}
	}
	return reaching
}

// TestGatewayAPIEdgeIsOneRouteToMain holds the manifest to the #1359 P3 shape:
// one rule, PathPrefix `/`, elitea-main as the only backend.
//
// Each refusal below is a way the old split edge could come back. A second
// rule or a second backend is a path decision made outside Go again, the shape
// that let #568 ship. A RegularExpression match is what the MCP carve-out
// needed before the relay took it over; Gateway API leaves its support to the
// implementation, so a manifest that leans on one routes differently per
// Gateway. A match narrowed by a header, method or query parameter forwards
// less than its path says.
func TestGatewayAPIEdgeIsOneRouteToMain(t *testing.T) {
	if len(gatewayAPIEdgeFiles) == 0 {
		t.Fatal("gatewayAPIEdgeFiles is empty, so this gate checked nothing. It must name the checked-in Gateway-API edge.")
	}
	root := repoRoot(t)
	for _, relative := range gatewayAPIEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			routes := parseGatewayAPIEdge(t, root, relative)
			if len(routes) != 1 {
				t.Fatalf("%s holds %d HTTPRoutes; the browser edge is ONE route to %s, which relays the SPA itself", relative, len(routes), protectedService)
			}
			rules := routes[0].Spec.Rules
			if len(rules) != 1 {
				t.Fatalf("%s: the route has %d rules; want one `/` rule. A per-path rule list is a second copy of the Go router's routing that drifts (#568)", relative, len(rules))
			}
			rule := rules[0]
			if len(rule.BackendRefs) != 1 || rule.BackendRefs[0].Name != protectedService || rule.BackendRefs[0].Port != 8080 {
				t.Errorf("%s: the rule's backends are %+v; want exactly %s:8080. elitea-main relays /app and /docs to elitea-web itself (ELITEA_WEB_UPSTREAM)", relative, rule.BackendRefs, protectedService)
			}
			if len(rule.Matches) != 1 {
				t.Fatalf("%s: the rule has %d matches; want the single PathPrefix `/`", relative, len(rule.Matches))
			}
			match := rule.Matches[0]
			if match.Path.Type == "RegularExpression" {
				t.Errorf("%s: a RegularExpression match (%q). The MCP carve-out is decided by internal/api/webedge now; a regular expression here is implementation-specific and routes differently per Gateway", relative, match.Path.Value)
			}
			if match.Path.Type != "PathPrefix" || match.Path.Value != "/" {
				t.Errorf("%s: the match is %s %q; want PathPrefix `/`, so every path reaches the Go edge", relative, match.Path.Type, match.Path.Value)
			}
			if len(match.Headers) > 0 || len(match.QueryParams) > 0 || match.Method != "" {
				t.Errorf("%s: the match also requires a header, a method or a query parameter, so it forwards less than `/`", relative)
			}
			for _, filter := range rule.Filters {
				if filter.Type != "RequestHeaderModifier" {
					t.Errorf("%s: the rule carries a %s filter. A redirect or rewrite here pre-empts the Go edge (which owns `/` → `/app/`), and an ExtensionRef in front of /healthz answers probes with a login redirect (#569)", relative, filter.Type)
				}
			}
		})
	}
}

// removedHeaders returns the names one rule deletes on the way in.
func removedHeaders(rule gatewayRule) []string {
	var removed []string
	for _, filter := range rule.Filters {
		if filter.Type != "RequestHeaderModifier" {
			continue
		}
		removed = append(removed, filter.RequestHeaderModifier.Remove...)
	}
	sort.Strings(removed)
	return removed
}

// assertedHeaders returns the names one rule SETS or ADDS. A set or an add is
// the opposite of the control: it makes the edge assert that header for every
// caller.
func assertedHeaders(rule gatewayRule) []string {
	var asserted []string
	for _, filter := range rule.Filters {
		if filter.Type != "RequestHeaderModifier" {
			continue
		}
		for _, header := range filter.RequestHeaderModifier.Set {
			asserted = append(asserted, header.Name)
		}
		for _, header := range filter.RequestHeaderModifier.Add {
			asserted = append(asserted, header.Name)
		}
	}
	return asserted
}

func containsFold(names []string, wanted string) bool {
	for _, name := range names {
		if strings.EqualFold(name, wanted) {
			return true
		}
	}
	return false
}

// TestGatewayAPIEdgeStripsClientIdentity carries #326 and #368 onto the cluster
// edge.
//
// The removal is defence in depth since #1359: elitea-main deletes the same
// names itself (apimw.StripClientIdentityHeaders) and reads no X-Auth-* name as
// an identity. The floor stays, so the list cannot be shortened by accident
// while something between the Gateway and elitea-main could still read one.
func TestGatewayAPIEdgeStripsClientIdentity(t *testing.T) {
	root := repoRoot(t)

	// The floor on the required list itself, for the reason the loops give: an
	// empty list makes every check below vacuous.
	if len(requiredStrippedHeaders) == 0 {
		t.Fatal("requiredStrippedHeaders is empty, so this gate checked nothing.")
	}

	for _, relative := range gatewayAPIEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			rules := mainBackendRules(parseGatewayAPIEdge(t, root, relative))
			if len(rules) == 0 {
				t.Fatalf("%s has no rule with a backendRef to %q, so this gate checked nothing", relative, protectedService)
			}
			for index, rule := range rules {
				removed := removedHeaders(rule)
				var missing []string
				for _, name := range requiredStrippedHeaders {
					if !containsFold(removed, name) {
						missing = append(missing, name)
					}
				}
				if len(missing) > 0 {
					t.Errorf(
						"%s: rule %d reaches %q and does not remove %s.\n"+
							"elitea-main strips them as well, but this list is the floor every edge is held to: "+
							"extend it, never shorten it (#326).",
						relative, index, protectedService, strings.Join(missing, ", "),
					)
				}
				for _, name := range assertedHeaders(rule) {
					if containsFold(requiredStrippedHeaders, name) {
						t.Errorf(
							"%s: rule %d SETS or ADDS %s. Only `remove` deletes a header; a set makes this edge "+
								"assert that identity for every caller.",
							relative, index, name,
						)
					}
				}
			}
		})
	}
}

// TestGatewayAPIEdgeStripsAtLeastWhatTheComposeEdgesStrip is the #568 shape
// applied to the identity strip rather than to the routes.
//
// PR #564 widened the compose edge and the cluster edge stayed behind, and that
// went unnoticed because nothing compared the two. The compose edge's strip is
// elitea-main's own list now (apimw.ClientIdentityHeaders, #1359 P2). A name
// added there alone would repeat #568 exactly, and the cost would be higher:
// an identity header the compose stacks delete and the cluster forwards.
//
// The check is a SUPERSET, not equality. A name this edge deletes and the
// compose edges do not is safe, and the hop-marker exception is what makes the
// direction matter — see TestGatewayAPIEdgeForwardsTheHopMarker.
func TestGatewayAPIEdgeStripsAtLeastWhatTheComposeEdgesStrip(t *testing.T) {
	root := repoRoot(t)

	composeStripped := map[string]string{}
	for _, name := range apimw.ClientIdentityHeaders {
		composeStripped[name] = "apimw.ClientIdentityHeaders"
	}
	if len(composeStripped) == 0 {
		t.Fatal("the compose edge strips no header at all, so this comparison read nothing")
	}

	for _, relative := range gatewayAPIEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			rules := mainBackendRules(parseGatewayAPIEdge(t, root, relative))
			if len(rules) == 0 {
				t.Fatalf("%s has no rule with a backendRef to %q, so this gate checked nothing", relative, protectedService)
			}
			for index, rule := range rules {
				removed := removedHeaders(rule)
				var missing []string
				for name := range composeStripped {
					if !containsFold(removed, name) {
						missing = append(missing, name+" (from "+composeStripped[name]+")")
					}
				}
				if len(missing) > 0 {
					sort.Strings(missing)
					t.Errorf(
						"%s: rule %d does not remove %s, and a compose edge does.\n"+
							"The two edges front the same service. A name deleted on one and forwarded on the "+
							"other leaves the cluster open to exactly what the compose fix closed (#326, #568).",
						relative, index, strings.Join(missing, ", "),
					)
				}
			}
		})
	}
}

// TestGatewayAPIEdgeForwardsTheHopMarker points the OPPOSITE way, and it is the
// same exception edge_identity_strip_test.go carves for the compose edge
// (#164). The two must stay consistent: the marker survives EVERY edge, or the
// anti-circular-routing mechanism breaks on the edge that deletes it.
//
// The canonical circular route leaves the LLM gateway as an ordinary provider
// call and comes back in through a browser edge:
//
//	gateway -> provider (= the platform's own /llm) -> EDGE -> elitea-main -> gateway
//
// The marker is the only thing that separates that request from a first visit.
// An edge that removed it would leave the gateway stamping every outbound
// request and never recognising one coming back — hop detection armed in the
// code and dead on the wire, with every unit test in both modules still green.
//
// The exemption is safe because the marker is not identity. It grants nothing,
// recognising it REFUSES a request rather than admitting one, and detection
// keeps no state, so a harvested marker reaches no other request. See
// mustForwardHeaders in edge_identity_strip_test.go for the full argument; this
// test applies it to the surface #568 added.
func TestGatewayAPIEdgeForwardsTheHopMarker(t *testing.T) {
	root := repoRoot(t)

	if len(mustForwardHeaders) == 0 {
		t.Fatal("mustForwardHeaders is empty, so this gate checked nothing. It must name the LLM gateway's hop marker.")
	}

	for _, relative := range gatewayAPIEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			rules := mainBackendRules(parseGatewayAPIEdge(t, root, relative))
			if len(rules) == 0 {
				t.Fatalf("%s has no rule with a backendRef to %q, so this gate checked nothing", relative, protectedService)
			}
			for index, rule := range rules {
				removed := removedHeaders(rule)
				for _, forwarded := range mustForwardHeaders {
					if containsFold(removed, forwarded) {
						t.Errorf(
							"%s: rule %d removes %s.\n"+
								"This header must reach elitea-main untouched. The LLM gateway stamps it on every "+
								"outbound provider request and recognises it coming back, which is how a circular "+
								"route — gateway -> provider (= this platform's own /llm) -> this edge -> "+
								"elitea-main -> gateway — is contained on its first re-entry. An edge that deletes "+
								"it leaves the gateway unable to recognise its own traffic, with every unit test "+
								"still green.\n"+
								"The compose edge carries the same exception. Keep the edges consistent: "+
								"the marker survives all of them, or none of them protects the loop.",
							relative, index, forwarded,
						)
					}
					// A set is as bad as a remove here, and less obvious: it
					// gives every request the SAME marker value, so the gateway
					// either refuses all of them or none of them.
					if containsFold(assertedHeaders(rule), forwarded) {
						t.Errorf(
							"%s: rule %d SETS or ADDS %s. The edge must pass the caller's value through untouched; "+
								"a fixed value makes every request look identical to the gateway's hop detection.",
							relative, index, forwarded,
						)
					}
				}
			}
		})
	}
}

// chartIngressValues is the subset of the chart values this gate reads.
type chartIngressValues struct {
	Main struct {
		Ingress struct {
			StripIdentityHeaders []string `yaml:"stripIdentityHeaders"`
		} `yaml:"ingress"`
	} `yaml:"main"`
}

// TestChartHTTPRouteStripsClientIdentity covers the THIRD Gateway-API surface:
// the HTTPRoute the chart renders itself, from
// deploy/helm/elitea/templates/main/ingress.yaml.
//
// That template renders the same one-rule shape as the checked-in manifest, but
// its RequestHeaderModifier is built from a values list, and that list is a copy
// of the same control. Nothing read it before #568, so a shortened list would
// have shipped with `helm lint` green.
func TestChartHTTPRouteStripsClientIdentity(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, chartValuesFile))
	if err != nil {
		t.Fatalf(
			"read %s: %v.\nThe chart moved and this gate stopped gating; update chartValuesFile in this file.",
			chartValuesFile, err,
		)
	}
	var values chartIngressValues
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", chartValuesFile, err)
	}

	stripped := values.Main.Ingress.StripIdentityHeaders
	if len(stripped) == 0 {
		t.Fatalf(
			"%s: main.ingress.stripIdentityHeaders is empty or absent.\n"+
				"The rendered HTTPRoute then removes nothing, and only elitea-main's own strip is left (#326). "+
				"The key may also have been renamed, in which case this gate reads a path that no longer "+
				"exists — check the template before you change this test.",
			chartValuesFile,
		)
	}

	var missing []string
	for _, name := range requiredStrippedHeaders {
		if !containsFold(stripped, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf(
			"%s: main.ingress.stripIdentityHeaders does not name %s.\n"+
				"Extend that list; never shorten it.",
			chartValuesFile, strings.Join(missing, ", "),
		)
	}

	// The hop-marker exception, on the chart surface. See
	// TestGatewayAPIEdgeForwardsTheHopMarker for why this one name stays out of
	// every strip list.
	for _, forwarded := range mustForwardHeaders {
		if containsFold(stripped, forwarded) {
			t.Errorf(
				"%s: main.ingress.stripIdentityHeaders names %s.\n"+
					"The rendered HTTPRoute would delete the LLM gateway's hop marker, which disarms loop "+
					"detection on every install of this chart. It is not identity: it grants nothing, and "+
					"recognising it only refuses the caller's own request.",
				chartValuesFile, forwarded,
			)
		}
	}
}

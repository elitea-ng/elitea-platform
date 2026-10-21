// The Gateway-API browser edge (#568).
//
// WHAT WAS MISSING.
//
// edge_root_routes_test.go walks the real Go router and fails when a
// root-mounted route matches no rule at the browser edge. It read TWO files,
// both of them traefik, both of them compose-only (since deleted: elitea-main
// is the compose edge now, #1359 P2). The cluster ran neither.
// Its edge is a Gateway API HTTPRoute, it lived in the deployment repository
// alone, and no file here described it. So the walk could not reach it,
// `requiredWalkedFamilies` proved nothing about the cluster, and a green run
// was fully compatible with the cluster edge being a release behind.
//
// It was. PR #564 added four path prefixes to the compose edge. The cluster
// edge never got them, and project icons and author avatars stayed broken
// there for the whole 1.20.1 window. `/icons/1/a.png` answered the Gateway's
// own 404, byte-identical to an unrouted path.
//
// WHAT THIS FILE ADDS.
//
// deploy/gateway-api/httproute.yaml is now the reviewed source of that edge,
// and this file holds it to the same two rules the compose edge answers to:
//
//  1. every root-mounted Go route is forwarded, or excluded with a reason;
//  2. every rule that reaches elitea-main removes the caller's identity
//     headers, and none of them removes the LLM gateway's hop marker.
//
// The classification uses assertPatternsClassified and assertNoStaleExclusions
// from edge_root_routes_test.go. Sharing them is the point: ONE definition of
// "classified" now covers both edges, so widening it cannot reach the compose
// edge and leave the cluster behind — which is this defect.
//
// WHAT IT STILL DOES NOT COVER.
//
// The copy in the deployment repository. Nothing here reads the cluster, so a
// copy that stops tracking the file still deploys. Issue #568 names the second
// control for that gap: a post-deploy check of the root-mounted paths against
// the deployed host, discriminating on `vary: Origin`, which elitea-main sets
// and the Gateway's own 404 does not.
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
	"regexp"
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

// chartValuesFile holds the chart's own HTTPRoute posture. The chart renders a
// DIFFERENT topology from the manifest above — one `/` rule with elitea-main as
// the only backend — so the route classification does not apply to it. Its
// header-strip list is a third copy of the same security control, and
// TestChartHTTPRouteStripsClientIdentity holds that copy to the same floor.
const chartValuesFile = "deploy/helm/elitea/values.yaml"

// notAtTheGatewayAPIEdge names the router patterns the GATEWAY-API edge alone
// does not carry. notAtTheBrowserEdge in edge_root_routes_test.go holds what is
// true of every edge; an entry here is true of this edge only, and the gate
// fails when this edge does forward it after all.
var notAtTheGatewayAPIEdge = map[string]string{
	"/app/{projectID}/mcp": "the MCP server shares the SPA prefix and needs a path REGULAR EXPRESSION to be carved back out of it. " +
		"The compose edge matches it with a regular expression (internal/api/webedge.MCPPath). Gateway API v1 has Exact and PathPrefix as its portable path matches, and support for " +
		"RegularExpression is implementation-specific, so this copy does not claim it. An MCP client on this edge reaches the SPA " +
		"backend and gets its 404. Add a RegularExpression match when the Gateway of the deployment supports one, and delete this entry",
	"/app/{projectID}/mcp/*": "the same route, wildcard form",
}

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

// gatewayPathPrefixMatches implements the Gateway API PathPrefix match, which
// is NOT a string prefix: it matches on path element boundaries, so `/icons`
// matches `/icons/1/a.png` and never matches `/iconsfoo`. A gate that used
// strings.HasPrefix here would report a route as covered that the cluster
// answers with its own 404.
func gatewayPathPrefixMatches(prefix, requestPath string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}

// gatewayRulesForward answers whether any rule matches requestPath.
func gatewayRulesForward(t *testing.T, relative string, rules []gatewayRule, requestPath string) bool {
	t.Helper()
	for _, rule := range rules {
		for _, match := range rule.Matches {
			if len(match.Headers) > 0 || len(match.QueryParams) > 0 || match.Method != "" {
				t.Fatalf(
					"%s: a match on path %q also requires a header, a method or a query parameter.\n"+
						"This gate reads the path alone, so it would report the route as covered while the edge "+
						"forwards a narrower set. Teach the gate that field, or split the match.",
					relative, match.Path.Value,
				)
			}
			if match.Path.Value == "" {
				t.Fatalf(
					"%s: a match declares no path value.\n"+
						"Gateway API defaults an absent path to PathPrefix `/`, which forwards everything — "+
						"including /readyz and /startupz. Write the path out.",
					relative,
				)
			}
			switch match.Path.Type {
			case "Exact":
				if requestPath == match.Path.Value {
					return true
				}
			case "PathPrefix":
				if gatewayPathPrefixMatches(match.Path.Value, requestPath) {
					return true
				}
			case "RegularExpression":
				pattern, err := regexp.Compile(match.Path.Value)
				if err != nil {
					t.Fatalf("%s: the path regular expression %q does not compile: %v", relative, match.Path.Value, err)
				}
				if pattern.MatchString(requestPath) {
					return true
				}
			default:
				t.Fatalf(
					"%s: path match type %q is one this gate cannot read. Gateway API defines Exact, PathPrefix "+
						"and RegularExpression; an unread type makes this gate answer \"no match\" for a path the "+
						"edge does forward.",
					relative, match.Path.Type,
				)
			}
		}
	}
	return false
}

// TestGatewayAPIEdgeClassifiesEveryRootMountedGoRoute is the gate #568 asked
// for. It walks the real Go router and fails when a root-mounted route reaches
// no rule of the Gateway-API edge and carries no written reason.
func TestGatewayAPIEdgeClassifiesEveryRootMountedGoRoute(t *testing.T) {
	// The floor on this gate's own input. An empty list makes every loop below
	// iterate zero times and the whole gate report a clean tree.
	if len(gatewayAPIEdgeFiles) == 0 {
		t.Fatal("gatewayAPIEdgeFiles is empty, so this gate checked nothing. It must name the checked-in Gateway-API edge.")
	}

	patterns := walkRootMountedGoRoutes(t)
	root := repoRoot(t)

	// The union of the two exclusion lists. The shared one holds what is true
	// of every edge; the second holds what is true of this edge only.
	excluded := map[string]string{}
	for pattern, why := range notAtTheBrowserEdge {
		excluded[pattern] = why
	}
	for pattern, why := range notAtTheGatewayAPIEdge {
		excluded[pattern] = why
	}

	for _, relative := range gatewayAPIEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			routes := parseGatewayAPIEdge(t, root, relative)
			rules := mainBackendRules(routes)
			if len(rules) == 0 {
				t.Fatalf(
					"%s has no rule with a backendRef to %q, so this gate checked nothing.\n"+
						"Either the Service was renamed or the file changed shape; update protectedService in "+
						"edge_identity_strip_test.go.",
					relative, protectedService,
				)
			}
			forwards := func(t *testing.T, requestPath string) bool {
				return gatewayRulesForward(t, relative, rules, requestPath)
			}
			assertPatternsClassified(t, relative, patterns, forwards, excluded)
			assertNoStaleExclusions(t, relative, patterns, forwards, excluded)

			// The two lists edge_root_routes_test.go states by hand. They are a
			// readable floor under the walk, and they carry the reason each
			// path is public.
			for requestPath, why := range rootMountedGoPaths {
				if !forwards(t, requestPath) {
					t.Errorf("%s: no rule forwards %s (%s); the Gateway answers its own 404 for it", relative, requestPath, why)
				}
			}
			for requestPath, why := range notForwardedGoPaths {
				if forwards(t, requestPath) {
					t.Errorf("%s: a rule forwards %s (%s); this edge serves browsers and must not publish it", relative, requestPath, why)
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
// elitea-main accepts X-Auth-Type + X-Auth-ID as a finished identity from any
// source inside trusted_proxy_cidrs, and the pod network the Gateway sits on is
// inside them. The RequestHeaderModifier filter is the single file standing
// between the cluster and caller-chosen identity, and until #568 nothing read
// it.
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
							"elitea-main reads those names as a finished identity from a trusted-proxy source, and "+
							"the Gateway's pod network is one. Every name left in place is one a caller chooses for "+
							"itself (#326).",
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
// That template renders a different topology from the checked-in manifest — one
// `/` rule with elitea-main as the only backend — so the route classification
// above does not apply to it. Its RequestHeaderModifier is built from a values
// list, and that list is a copy of the same security control. Nothing read it
// before #568, so a shortened list would have shipped with `helm lint` green.
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
				"The rendered HTTPRoute then removes nothing, and a caller inside the pod network chooses its "+
				"own user id (#326). The key may also have been renamed, in which case this gate reads a path "+
				"that no longer exists — check the template before you change this test.",
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

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// GET /elitea_core/toolkits/prompt_lib/{projectID} is the toolkit TYPE
// catalogue: a map of toolkit type name -> settings JSON Schema. It is a
// different resource from GET /elitea_core/tools/prompt_lib/{projectID}, the
// INSTANCE list.
//
// Three independent sources agree on that split:
//
//	api/openapi/v2.yaml:6220-6239   listToolkits -> ToolkitTypeSchemas
//	                                (and listToolkitInstances for /tools/)
//	apps/elitea-web/src/shared/api/generated/toolkits/toolkits.ts:562
//	                                getListToolkitsUrl -> /elitea_core/toolkits/...
//	                                typed listToolkitsResponse200.data: ToolkitTypeSchemas
//	legacy elitea_core api/v2/toolkits.py -> get_toolkit_schemas / get_mcp_schemas
//	                                (api/v2/tools.py is the instance list)
//
// Until #129 both /toolkits/ registrations pointed at toolkitHandler.List, so
// the endpoint returned {"rows":[],"total":0} and ListTypeSchemas — a complete
// handler backed by a populated map — had no route anywhere in the binary. The
// MCP create screen filters the response for mcp-flavoured keys, found none in
// an instance envelope, and rendered "Still no local MCP available" forever.
//
// This is the route-level twin of the nil-gate class in #126/#115: a working
// handler that nothing reaches. router_nil_gate_test.go cannot see it, because
// there is no `cfg.X != nil` to inspect — the route is present, it just points
// at the wrong function.
func TestToolkitsRouteServesTheTypeCatalogueNotTheInstanceList(t *testing.T) {
	// The gated branch's RequireProjectAccess needs a live pool; take the
	// un-gated branch so the response body is observable. The gated branch's
	// registration is covered by the source-level test below.
	t.Setenv("FEATURE_FLAG_TOOLKIT_PROJECT_ACCESS", "false")
	snapshot, err := runtimecomposition.LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit schema snapshot: %v", err)
	}
	router := NewRouter(RouterConfig{
		SkillsRepo:             struct{ v2skills.Repository }{},
		AuthValidator:          testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:     testPrincipalValidator{},
		ToolkitArgumentSchemas: snapshot,
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/toolkits/prompt_lib/1", nil)))
	// ListTypeSchemas serves a package-level map and never touches the pool, so
	// 200 is reachable with no database. A 500 here means the route reached a
	// handler that did query — i.e. it is still pointing at List.
	if response.Code != http.StatusOK {
		t.Fatalf("GET /elitea_core/toolkits/prompt_lib/1 = %d, want 200 (a DB-independent handler): %s",
			response.Code, strings.TrimSpace(response.Body.String()))
	}

	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, response.Body.String())
	}

	// The instance envelope must not be what came back. This is the assertion
	// that actually fails when the route points at List.
	if _, ok := body["rows"]; ok {
		t.Errorf("response carries the instance-list envelope %q, so /toolkits/ is still wired to List", response.Body.String())
	}

	// And the type catalogue must really be there — not merely "not rows".
	// A single type is enough to pin the shape; "github" is one of the eight
	// entries in toolkits.toolkitTypeSchemas.
	github, ok := body["github"].(map[string]any)
	if !ok {
		t.Fatalf("response has no \"github\" toolkit type: %s", response.Body.String())
	}
	properties, ok := github["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the \"github\" entry is not a JSON-Schema-shaped settings descriptor: %v", github)
	}

	// The catalogue's payload is the per-tool ARGUMENT schemas the web client
	// renders forms from, and they must arrive through the route, not merely
	// exist in the snapshot. Until the SDK snapshot started carrying them, every
	// one was the placeholder {"type":"object"} — which is why this asserts on
	// real content. create_issue is a github tool at SDK revision b5113a1; its
	// title argument is the one a form needs to draw a text field.
	selectedTools, ok := properties["selected_tools"].(map[string]any)
	if !ok {
		t.Fatalf("the \"github\" entry declares no selected_tools property: %v", properties)
	}
	argsSchemas, ok := selectedTools["args_schemas"].(map[string]any)
	if !ok {
		t.Fatalf("github selected_tools carries no args_schemas: %v", selectedTools)
	}
	createIssue, ok := argsSchemas["create_issue"].(map[string]any)
	if !ok {
		t.Fatalf("github exposes no create_issue argument schema: %v", argsSchemas)
	}
	createIssueProperties, ok := createIssue["properties"].(map[string]any)
	if !ok {
		t.Fatalf("create_issue is still a placeholder with no properties: %v", createIssue)
	}
	if _, ok := createIssueProperties["title"].(map[string]any); !ok {
		t.Errorf("create_issue takes no title argument: %v", createIssueProperties)
	}
}

// The catalogue's argument schemas reach the handler only if the composition
// root fills RouterConfig.ToolkitArgumentSchemas and router.go passes it on.
// Neither step is nil-gated, so nothing 404s or errors when one is missing: the
// endpoint keeps answering 200 with settings-only schemas and every tool form in
// the web client silently renders empty — the exact defect this wiring fixes.
// That is the invisible-wiring class of #115/#123/#126, so it gets a source-level
// gate like they did.
func TestToolkitArgumentSchemasAreWiredFromTheCompositionRoot(t *testing.T) {
	t.Parallel()

	if !regexp.MustCompile(`v2toolkits\.WithArgumentSchemas\(cfg\.ToolkitArgumentSchemas\)`).
		MatchString(readRouterSource(t)) {
		t.Error("router.go builds the toolkit handler without cfg.ToolkitArgumentSchemas, " +
			"so GET /elitea_core/toolkits/prompt_lib/{projectID} serves no tool argument schemas")
	}

	main, err := os.ReadFile(filepath.Join("..", "..", "cmd", "elitea-main", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !regexp.MustCompile(`ToolkitArgumentSchemas:\s*toolkitArgumentSchemas`).Match(main) {
		t.Error("cmd/elitea-main/main.go never assigns RouterConfig.ToolkitArgumentSchemas, " +
			"so the pinned SDK snapshot never reaches the toolkit type catalogue in production")
	}
}

// Internal MCP must not construct a reduced toolkit handler of its own. The
// shared instance is what keeps MCP create/update behind the same dynamic MCP
// schemas, secret sealing, credential-reference validation and guardrail
// policy as the REST/UI entry point.
func TestInternalMCPToolkitBuilderReusesTheComposedRESTHandler(t *testing.T) {
	t.Parallel()

	source := readRouterSource(t)
	for description, pattern := range map[string]string{
		"single composed handler":    `toolkitHandler := newToolkitHandler\(cfg, prebuiltMCPStore\)`,
		"MCP mount receives handler": `mountMCPServerRoutes\(r, cfg\.Pool, authenticate, cfg\.MCPAgentStart, toolkitHandler\)`,
		"MCP handler option":         `v2mcp\.WithInternalToolkitHandler\(toolkitHandler\)`,
	} {
		if !regexp.MustCompile(pattern).MatchString(source) {
			t.Errorf("router.go is missing %s; Internal MCP and REST toolkit mutations can diverge", description)
		}
	}
}

// The instance list keeps its own route. Moving ListTypeSchemas onto /toolkits/
// must not be "fixed" by moving List off /tools/ as well: the web client calls
// both (toolkits.ts:562 and :764).
func TestToolsRouteStillServesTheToolkitInstanceList(t *testing.T) {
	t.Parallel()

	source := readRouterSource(t)
	if !regexp.MustCompile(`Get\("/tools/prompt_lib/\{projectID\}", toolkitHandler\.List\)`).MatchString(source) {
		t.Error("no GET /tools/prompt_lib/{projectID} -> toolkitHandler.List registration remains in router.go")
	}
}

// A source-level guard, because the gated branch (the production default)
// cannot be exercised behaviourally without a database.
//
// It used to demand TWO registrations: the FEATURE_FLAG_TOOLKIT_PROJECT_ACCESS
// block was written out once per branch, so a fix applied to one copy and not
// the other was the mistake to expect — and this test was the only thing that
// would have caught it. #313 removed the duplication rather than keeping the
// two copies in step: one registration list now serves both settings of the
// flag, which the middleware factory selects. So the count is one, and the
// property the old test approximated ("no copy of this route is wired to the
// instance list") is now structural.
//
// The count is still asserted, not just the handler name. A second registration
// reappearing would mean the branches were split again, and this file is where
// that has to be noticed.
func TestEveryToolkitsRouteRegistrationNamesListTypeSchemas(t *testing.T) {
	t.Parallel()

	source := readRouterSource(t)
	registrations := regexp.MustCompile(`Get\("/toolkits/prompt_lib/\{projectID\}", toolkitHandler\.(\w+)\)`).
		FindAllStringSubmatch(source, -1)

	if len(registrations) != 1 {
		t.Fatalf("found %d GET /toolkits/prompt_lib/{projectID} registrations in router.go, want 1 — "+
			"the feature-flag branches share one registration list since #313; if they were split "+
			"again, every route in the block needs the copy-versus-copy check this test used to make",
			len(registrations))
	}
	for _, registration := range registrations {
		if registration[1] != "ListTypeSchemas" {
			t.Errorf("%s routes the type catalogue at the wrong handler: got toolkitHandler.%s, want toolkitHandler.ListTypeSchemas",
				registration[0], registration[1])
		}
	}
}

func readRouterSource(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	return string(source)
}

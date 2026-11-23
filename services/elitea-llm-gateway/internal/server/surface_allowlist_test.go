// The HTTP twin of the gateway's internal-listener surface test (ADR-0059
// decision 7, "Internal paths"; story R1.9), run by ci-gateway.yml.
//
// This package's Server is the plain-HTTP health and metrics port: its mux
// routes and every listener site are pinned by the source scan in
// internal/grpcapi/surface_allowlist_test.go. What a caller can act on is the
// /llm chi router (api.NewRouterWithLog), which has no listener of its own:
// the GatewayService RPCs run it in process (grpcapi/bridge.go). This test
// walks the routes that router registers (chi.Walk) and compares them with the
// allowlist's `http | llm | ...` lines, both ways.
package server_test

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

const (
	surfaceAllowlistFile = "../grpcapi/internal_listener_allowlist.txt"
	// routerListener names the /llm chi router's http lines.
	routerListener = "llm"
)

// routerLine is one `http | llm | <METHOD pattern> | <writes> | <decision> |
// <reason>` line of the allowlist. The grpcapi test validates every field of
// every line; this reader takes the router's lines only.
type routerLine struct {
	route, writes, decision, reason string
	line                            int
}

func loadRouterLines(t *testing.T, file string) []routerLine {
	t.Helper()
	handle, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	var lines []routerLine
	scanner := bufio.NewScanner(handle)
	for number := 1; scanner.Scan(); number++ {
		fields := strings.Split(scanner.Text(), "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) != 6 || fields[0] != "http" || fields[1] != routerListener {
			continue
		}
		lines = append(lines, routerLine{route: fields[2], writes: fields[3], decision: fields[4], reason: fields[5], line: number})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatalf("%s has no %q lines", file, "http | "+routerListener)
	}
	return lines
}

// registeredRoutes is every "METHOD pattern" the router registers, with how
// often.
func registeredRoutes(t *testing.T, router http.Handler) map[string]int {
	t.Helper()
	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("the /llm router is a %T, not a chi router: walk it some other way", router)
	}
	found := map[string]int{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[method+" "+route]++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// checkRouter compares the registered routes with the router lines, both
// ways. A route that can change administration data must be decided by
// elitea-main's own route (main-route); the gateway has none.
func checkRouter(found map[string]int, lines []routerLine) []string {
	var problems []string
	listed := map[string]bool{}
	for _, line := range lines {
		listed[line.route] = true
		switch {
		case line.reason == "":
			problems = append(problems, fmt.Sprintf("allowlist line %d: %s has no reason", line.line, line.route))
		case line.writes == "admin" && line.decision != "main-route":
			problems = append(problems, fmt.Sprintf("allowlist line %d: %s can change administration data without an authorization decision in elitea-main (decision %s; it must be main-route)", line.line, line.route, line.decision))
		}
		if found[line.route] == 0 {
			problems = append(problems, fmt.Sprintf("allowlist line %d: the /llm router does not register %s (remove the line)", line.line, line.route))
		}
	}
	for route, count := range found {
		if !listed[route] || count != 1 {
			problems = append(problems, fmt.Sprintf("the /llm router registers %s (%d times), which is not on the allowlist once: review what it writes, what decides it and who calls it, and list it", route, count))
		}
	}
	slices.Sort(problems)
	return problems
}

// llmRouter is the router cmd/elitea-llm-gateway hands the gRPC listener.
// The routes do not depend on the handler's wiring.
func llmRouter() http.Handler {
	return api.NewRouterWithLog(llmproxy.NewHandler(nil, nil, nil), nil)
}

func containsRouterProblem(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, want) })
}

func TestGatewayRouterRegistersOnlyReviewedRoutes(t *testing.T) {
	for _, problem := range checkRouter(registeredRoutes(t, llmRouter()), loadRouterLines(t, surfaceAllowlistFile)) {
		t.Error(problem)
	}
}

// The fixtures: the check must fail on what it exists to catch.

func TestTheGatewayRouterCheckFailsOnAnUnlistedRoute(t *testing.T) {
	router := chi.NewRouter()
	router.Mount("/", llmRouter())
	router.Post("/llm/v1/admin/providers", func(http.ResponseWriter, *http.Request) {})
	problems := checkRouter(registeredRoutes(t, router), loadRouterLines(t, surfaceAllowlistFile))
	if !containsRouterProblem(problems, "POST /llm/v1/admin/providers") {
		t.Fatalf("an unlisted fixture route passed: %v", problems)
	}
}

func TestTheGatewayRouterCheckFailsOnAnAdministrationWriteMainDoesNotDecide(t *testing.T) {
	lines := []routerLine{{route: "PUT /llm/v1/providers", writes: "admin", decision: "workload", reason: "fixture", line: 1}}
	problems := checkRouter(map[string]int{"PUT /llm/v1/providers": 1}, lines)
	if !containsRouterProblem(problems, "without an authorization decision in elitea-main") {
		t.Fatalf("an administration write decided by a workload certificate passed: %v", problems)
	}
}

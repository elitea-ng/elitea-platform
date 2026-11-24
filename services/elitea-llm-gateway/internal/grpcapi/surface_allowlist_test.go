// The internal-listener surface of the LLM gateway (ADR-0059 decision 7,
// "Internal paths"; story R1.9). The gateway is outside go.work, so the
// workspace tests of R1.8 cannot see it; ci-gateway.yml runs these. The
// gateway reads provider, policy and model data from PostgreSQL and writes no
// administration data. These tests pin, against the reviewed allowlist
// (internal_listener_allowlist.txt), its gRPC listener (GatewayService) and
// every listener and registration site and health-mux route of the module's
// source. The HTTP twin, internal/server/surface_allowlist_test.go, pins the
// routes of the /llm chi router the RPCs run. The checks are in
// internal_listener_checks_test.go, a copy of R1.8's.
package grpcapi_test

import (
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/grpcapi"
)

const (
	surfaceAllowlistFile = "internal_listener_allowlist.txt"
	surfaceModuleRoot    = "../.."
	surfaceArchitecture  = "../../../../docs/ARCHITECTURE.md"
	surfaceServiceName   = "elitea-llm-gateway"
	surfaceModulePath    = "github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway"
	// routerListener names the /llm chi router's http lines. They name no
	// file: the HTTP twin compares them with chi.Walk, not the source scan.
	routerListener = "llm"
	// The reference copy of the checks: elitea-main's, in the Go workspace.
	surfaceChecksReference = "../../../elitea-main/internal/transport/internal_listener_checks_test.go"
)

// composedGatewayListener builds the listener through its real constructors,
// as cmd/elitea-llm-gateway/grpc_listener.go does, and reports what it
// serves. Nothing listens: GetServiceInfo reads the registrations.
func composedGatewayListener(t *testing.T) map[string]map[string]grpc.ServiceInfo {
	t.Helper()
	service, err := grpcapi.NewService(grpcapi.Config{LLM: http.NotFoundHandler(), Status: http.NotFoundHandler()})
	if err != nil {
		t.Fatal(err)
	}
	allow, err := grpcapi.ParseAllowlist("dns:elitea-main")
	if err != nil {
		t.Fatal(err)
	}
	server, err := grpcapi.NewServer("127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert}, allow, service)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]map[string]grpc.ServiceInfo{"gateway": server.ServiceInfo()}
}

func TestGatewayListenerServesOnlyReviewedMethods(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkServed(composedGatewayListener(t), list) {
		t.Error(problem)
	}
}

// The source scan reads every non-test file of the module: the health and
// metrics mux, the HTTP and gRPC listeners and their registrations. The
// router's lines name no file; they obey the same rule, and the HTTP twin
// compares them with the routes.
func TestGatewayListenerAndRegistrationSitesAreReviewed(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	var scanned []surfaceEntry
	for _, entry := range list {
		if entry.kind == "http" && entry.listener == routerListener {
			if problem := ruleProblem(entry); problem != "" {
				t.Error(problem)
			}
			continue
		}
		scanned = append(scanned, entry)
	}
	for _, problem := range checkSource(scanSource(t, surfaceModuleRoot, scanned), scanned) {
		t.Error(problem)
	}
}

// Every reviewed line gives a reason: who calls it and what it writes.
// loadAllowlist refuses an empty field; this also refuses a one-word one and
// an allowlist that lost a section.
func TestGatewayAllowlistGivesAReasonPerEntry(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	kinds := map[string]int{}
	for _, entry := range list {
		kinds[entry.kind]++
		if (entry.kind == "grpc" || entry.kind == "http") && len(strings.Fields(entry.caller)) < 2 {
			t.Errorf("allowlist line %d: %s needs a reason (who calls it and what it writes), not %q", entry.line, entry.name, entry.caller)
		}
	}
	if kinds["grpc"] == 0 || kinds["http"] == 0 || kinds["site"] == 0 {
		t.Fatalf("the allowlist lost a section: %v", kinds)
	}
}

// The handlers run under cmd/elitea-llm-gateway, which composes every
// package; the scan reads this package, main and every package of the module
// they import. The gateway has no administration permission mode at all.
func TestGatewayHandlersNeverResolveTheAdministrationMode(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range scanAdministrationModeClosure(t, surfaceModuleRoot, surfaceModulePath, []string{".", "../../cmd/elitea-llm-gateway"}, list) {
		t.Error(problem)
	}
}

func TestArchitectureListsTheGatewayAdministrationWrites(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkArchitecture(t, surfaceArchitecture, surfaceServiceName, list) {
		t.Error(problem)
	}
}

// The gateway twin of elitea-main's TestTheListenerChecksCopiesAreAlike: a
// gateway-only change runs ci-gateway, not ci-go, so the gateway checks its
// own copy too.
func TestTheGatewayListenerChecksCopiesAreAlike(t *testing.T) {
	body := func(file string) string {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, ok := strings.Cut(string(raw), "\n")
		if !ok {
			t.Fatalf("%s is empty", file)
		}
		return rest
	}
	if body("internal_listener_checks_test.go") != body(surfaceChecksReference) {
		t.Errorf("internal_listener_checks_test.go differs from %s beyond the package clause: copy the fix", surfaceChecksReference)
	}
}

// The fixtures: each check must fail on what it exists to catch.

func TestTheGatewaySurfaceCheckFailsOnAnUnlistedMethod(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	served := composedGatewayListener(t)
	served["gateway"]["fixture.admin.v1.AdminService"] = grpc.ServiceInfo{Methods: []grpc.MethodInfo{{Name: "SetProviderPolicy"}}}
	problems := checkServed(served, list)
	if !containsSurfaceProblem(problems, "/fixture.admin.v1.AdminService/SetProviderPolicy") {
		t.Fatalf("an unlisted fixture method passed: %v", problems)
	}
}

func TestTheGatewaySurfaceCheckFailsOnAnAdministrationWriteMainDoesNotDecide(t *testing.T) {
	list := []surfaceEntry{{kind: "grpc", listener: "gateway", name: "/fixture.admin.v1.AdminService/SetProviderPolicy", writes: "admin", decision: "workload", caller: "elitea-main", line: 1}}
	served := map[string]map[string]grpc.ServiceInfo{"gateway": {
		"fixture.admin.v1.AdminService": {Methods: []grpc.MethodInfo{{Name: "SetProviderPolicy"}}},
	}}
	if problems := checkServed(served, list); !containsSurfaceProblem(problems, "without an authorization decision in elitea-main") {
		t.Fatalf("an administration write decided by a workload certificate passed: %v", problems)
	}
}

func TestTheGatewayAdministrationModeScanFailsOnAFixtureHandler(t *testing.T) {
	if problems := scanAdministrationMode(t, filepath.Join("testdata", "adminmode")); len(problems) != 4 {
		t.Fatalf("want two constants and two literals flagged, got %v", problems)
	}
}

func TestTheGatewaySourceCheckFailsOnUnreviewedListeners(t *testing.T) {
	list := loadAllowlist(t, filepath.Join("testdata", "listenersurface", "allowlist.txt"))
	problems := checkSource(scanSource(t, filepath.Join("testdata", "listenersurface"), list), list)
	for _, want := range []string{
		"RegisterFixtureAdminServiceServer",
		"grpc.NewServer occurs 2 times, not 1",
		"POST /admin/roles",
		"server.go:registerMore DELETE /admin/users",
		"server.go:registerDefault PUT /admin/policy",
		"server.go:listenRaw net.Listen",
		"no helper directory fixturetest",
	} {
		if !containsSurfaceProblem(problems, want) {
			t.Errorf("the source check missed %s: %v", want, problems)
		}
	}
}

func containsSurfaceProblem(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, want) })
}

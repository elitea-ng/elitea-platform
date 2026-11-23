// The internal-listener surface of the sub-application host (ADR-0059
// decision 7, "Internal paths"; story R1.8). The host reads policy from
// PostgreSQL or receives it from elitea-main and writes no administration
// data. These tests pin its gRPC listener (the provider service and the
// platform operations Main calls), its admin HTTP port and every listener or
// registration site of the module against the reviewed allowlist
// (internal_listener_allowlist.txt); the checks are in
// internal_listener_checks_test.go.
package spi_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi/spitest"
)

const (
	surfaceModuleRoot    = "../.."
	surfaceAllowlistFile = "internal_listener_allowlist.txt"
	surfaceArchitecture  = "../../../../docs/ARCHITECTURE.md"
	surfaceServiceName   = "elitea-subapp-host"
	surfaceModulePath    = "github.com/elitea-ng/elitea-platform/services/elitea-subapp-host"
)

// surfaceRunner has both per-project and per-toolkit data, so the listener
// registers every platform operation.
type surfaceRunner struct{ spi.Runner }

func (surfaceRunner) DeleteProject(context.Context, int32) (*spi.ProjectDeletion, error) {
	return &spi.ProjectDeletion{}, nil
}

func (surfaceRunner) DeleteToolkit(context.Context, int32, int32) (*spi.ToolkitDeletion, error) {
	return &spi.ToolkitDeletion{}, nil
}

func composedHostListener(t *testing.T) map[string]map[string]grpc.ServiceInfo {
	t.Helper()
	server := echoServer(t, "", 0, surfaceRunner{Runner: echo.App(0).Runner})
	listener, err := spi.NewGRPCServer(server, []string{"elitea-main"}, spitest.NewPKI(t).ServerTLS(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]map[string]grpc.ServiceInfo{"platform": listener.GetServiceInfo()}
}

func TestHostListenerServesOnlyReviewedMethods(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkServed(composedHostListener(t), list) {
		t.Error(problem)
	}
}

func TestHostListenerAndRegistrationSitesAreReviewed(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkSource(scanSource(t, surfaceModuleRoot, list), list) {
		t.Error(problem)
	}
}

// The host's handlers live in internal/spi; the scan also reads every package
// of the module they import.
func TestHostHandlersNeverResolveTheAdministrationMode(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range scanAdministrationModeClosure(t, surfaceModuleRoot, surfaceModulePath, []string{"."}, list) {
		t.Error(problem)
	}
}

func TestArchitectureListsTheHostAdministrationWrites(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkArchitecture(t, surfaceArchitecture, surfaceServiceName, list) {
		t.Error(problem)
	}
}

func TestTheHostSurfaceCheckFailsOnAnUnlistedMethod(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	served := composedHostListener(t)
	served["platform"]["fixture.admin.v1.AdminService"] = grpc.ServiceInfo{Methods: []grpc.MethodInfo{{Name: "SetProviderPolicy"}}}
	problems := checkServed(served, list)
	if !containsSurfaceProblem(problems, "/fixture.admin.v1.AdminService/SetProviderPolicy") {
		t.Fatalf("an unlisted fixture method passed: %v", problems)
	}
}

func TestTheHostAdministrationModeScanFailsOnAFixtureHandler(t *testing.T) {
	if problems := scanAdministrationMode(t, filepath.Join("testdata", "adminmode")); len(problems) != 4 {
		t.Fatalf("want two constants and two literals flagged, got %v", problems)
	}
}

func TestTheHostSourceCheckFailsOnUnreviewedListeners(t *testing.T) {
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
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return true
		}
	}
	return false
}

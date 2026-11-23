package main

// The internal-listener surface of elitea-scheduler (ADR-0059 decision 7,
// "Internal paths"; story R1.8). The scheduler reads policy from PostgreSQL
// and serves no gRPC; its one listener is the health port. These tests pin
// every listener site and HTTP route of the module against the reviewed
// allowlist (internal_listener_allowlist.txt); the checks are in
// internal_listener_checks_test.go.

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
)

const (
	surfaceModuleRoot    = "../.."
	surfaceModulePath    = "github.com/elitea-ng/elitea-platform/services/elitea-scheduler"
	surfaceAllowlistFile = "internal_listener_allowlist.txt"
	surfaceArchitecture  = "../../../../docs/ARCHITECTURE.md"
	surfaceServiceName   = "elitea-scheduler"
)

// TestSchedulerServesNoGRPC holds the scheduler to no gRPC method: the
// allowlist may name none, and a grpc.NewServer in the source fails the site
// check below until a composed-listener comparison is added here.
func TestSchedulerServesNoGRPC(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkServed(map[string]map[string]grpc.ServiceInfo{}, list) {
		t.Error(problem)
	}
}

func TestSchedulerListenerSitesAndRoutesAreReviewed(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkSource(scanSource(t, surfaceModuleRoot, list), list) {
		t.Error(problem)
	}
}

// The health handlers are composed here (cmd/elitea-scheduler); the scan also
// reads every package of the module they import.
func TestSchedulerHandlersNeverResolveTheAdministrationMode(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range scanAdministrationModeClosure(t, surfaceModuleRoot, surfaceModulePath, []string{"."}, list) {
		t.Error(problem)
	}
}

func TestArchitectureListsTheSchedulerAdministrationWrites(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkArchitecture(t, surfaceArchitecture, surfaceServiceName, list) {
		t.Error(problem)
	}
}

func TestTheSchedulerAdministrationModeScanFailsOnAFixtureHandler(t *testing.T) {
	if problems := scanAdministrationMode(t, filepath.Join("testdata", "adminmode")); len(problems) != 4 {
		t.Fatalf("want two constants and two literals flagged, got %v", problems)
	}
}

func TestTheSchedulerSourceCheckFailsOnUnreviewedListeners(t *testing.T) {
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
		found := false
		for _, problem := range problems {
			found = found || strings.Contains(problem, want)
		}
		if !found {
			t.Errorf("the source check missed %s: %v", want, problems)
		}
	}
}

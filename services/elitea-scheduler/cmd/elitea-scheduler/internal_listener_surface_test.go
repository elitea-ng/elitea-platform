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
)

const (
	surfaceModuleRoot    = "../.."
	surfaceAllowlistFile = "internal_listener_allowlist.txt"
	surfaceArchitecture  = "../../../../docs/ARCHITECTURE.md"
	surfaceServiceName   = "elitea-scheduler"
)

func TestSchedulerListenerSitesAndRoutesAreReviewed(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, entry := range list {
		if entry.kind == "grpc" {
			t.Errorf("allowlist line %d: the scheduler serves no gRPC; a gRPC listener here needs the composed-listener comparison first", entry.line)
		}
	}
	for _, problem := range checkSource(scanSource(t, surfaceModuleRoot), list) {
		t.Error(problem)
	}
}

func TestSchedulerHandlersNeverResolveTheAdministrationMode(t *testing.T) {
	for _, dir := range []string{".", filepath.Join(surfaceModuleRoot, "internal", "health"), filepath.Join(surfaceModuleRoot, "internal", "budgetwriteback")} {
		for _, problem := range scanAdministrationMode(t, dir) {
			t.Error(problem)
		}
	}
}

func TestArchitectureListsTheSchedulerAdministrationWrites(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	for _, problem := range checkArchitecture(t, surfaceArchitecture, surfaceServiceName, list) {
		t.Error(problem)
	}
}

func TestTheSchedulerSourceCheckFailsOnAnUnlistedListener(t *testing.T) {
	list := loadAllowlist(t, surfaceAllowlistFile)
	problems := checkSource(scanSource(t, filepath.Join("testdata", "listenersurface")), list)
	for _, want := range []string{"grpc.NewServer", "RegisterFixtureAdminServiceServer", "POST /admin/roles"} {
		found := false
		for _, problem := range problems {
			found = found || strings.Contains(problem, want)
		}
		if !found {
			t.Errorf("the source check missed %s: %v", want, problems)
		}
	}
}

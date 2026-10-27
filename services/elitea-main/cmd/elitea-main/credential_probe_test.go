package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// Flag off: no probe handler, so no probe route; nothing is dialled.
func TestCredentialProbeBuildsNothingWithTheFlagOff(t *testing.T) {
	handler, err := credentialProbeAdmin(profile.LegacyAdmission(false), nil)
	if err != nil || handler != nil {
		t.Fatalf("flag off built handler=%v err=%v", handler, err)
	}
}

// Flag on: the handler is built; a missing database refuses boot.
func TestCredentialProbeBuildsTheHandlerWithTheFlagOn(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	if _, err := credentialProbeAdmin(governed, nil); err == nil {
		t.Fatal("flag on without a database was accepted")
	}
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	handler, err := credentialProbeAdmin(governed, pool)
	if err != nil || handler == nil {
		t.Fatalf("flag on: handler=%v err=%v", handler, err)
	}
}

package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

type noPins struct{}

func (noPins) ReadPin(context.Context, string) (profile.Pin, error) {
	return profile.Pin{}, profile.ErrPinNotFound
}

// Flag off (#1116): no decider is built and no grant admin handler exists,
// so the router mounts no grant route (router_credential_grants_test.go
// proves a nil handler answers 404). Nothing is dialled: the pool may be nil.
func TestCredentialGrantGovernanceBuildsNothingWithTheFlagOff(t *testing.T) {
	decider, handler, err := credentialGrantGovernance(profile.LegacyAdmission(false), nil, nil)
	if err != nil || decider != nil || handler != nil {
		t.Fatalf("flag off built decider=%v handler=%v err=%v", decider, handler, err)
	}
}

// Flag on: the decider is built with all eight gates and the handler
// exists; a missing database refuses boot.
func TestCredentialGrantGovernanceBuildsTheDeciderWithTheFlagOn(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	if _, _, err := credentialGrantGovernance(governed, nil, noPins{}); err == nil {
		t.Fatal("flag on without a database was accepted")
	}
	// pgxpool.New does not connect; construction must not need a live database.
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	decider, handler, err := credentialGrantGovernance(governed, pool, noPins{})
	if err != nil || decider == nil || handler == nil {
		t.Fatalf("flag on: decider=%v handler=%v err=%v", decider, handler, err)
	}
	if got := len(decider.GateNames()); got != 8 {
		t.Fatalf("decider has %d gates, want 8", got)
	}
}

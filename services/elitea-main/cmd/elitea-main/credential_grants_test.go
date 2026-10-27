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
	decider, handler, err := credentialGrantGovernance(profile.LegacyAdmission(false), nil, nil, nil, nil)
	if err != nil || decider != nil || handler != nil {
		t.Fatalf("flag off built decider=%v handler=%v err=%v", decider, handler, err)
	}
}

// Flag on: the decider is built with all eight gates and the handler
// exists; a missing database refuses boot.
func TestCredentialGrantGovernanceBuildsTheDeciderWithTheFlagOn(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	if _, _, err := credentialGrantGovernance(governed, nil, noPins{}, nil, credentialOperationCatalog()); err == nil {
		t.Fatal("flag on without a database was accepted")
	}
	// pgxpool.New does not connect; construction must not need a live database.
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	decider, handler, err := credentialGrantGovernance(governed, pool, noPins{}, nil, credentialOperationCatalog())
	if err != nil || decider == nil || handler == nil {
		t.Fatalf("flag on: decider=%v handler=%v err=%v", decider, handler, err)
	}
	if got := len(decider.GateNames()); got != 8 {
		t.Fatalf("decider has %d gates, want 8", got)
	}
}

// Flag off (#1118): no broker is built, so RedeemCredential answers
// Unimplemented. Nothing is read: every dependency may be nil.
func TestCredentialBrokerBuildsNothingWithTheFlagOff(t *testing.T) {
	broker, err := credentialBrokerAtBoot(profile.LegacyAdmission(true), func(string) (string, bool) {
		t.Fatal("the flag-off broker composition read the environment")
		return "", false
	}, nil, nil, nil, nil, nil, nil, nil)
	if err != nil || broker != nil {
		t.Fatalf("flag off built broker=%v err=%v", broker, err)
	}
}

// Flag on: a missing dependency refuses boot rather than serving a broker
// that cannot decide.
func TestCredentialBrokerRefusesMissingDependenciesWithTheFlagOn(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	if _, err := credentialBrokerAtBoot(governed, func(string) (string, bool) { return "", false },
		nil, nil, noPins{}, nil, nil, credentialOperationCatalog(), nil); err == nil {
		t.Fatal("flag on without a decider or a database was accepted")
	}
}

// The catalogue seam serves the built-in adapter descriptors: their
// operations and material keys only, nothing else.
func TestCredentialOperationCatalogIsTheBuiltinDescriptors(t *testing.T) {
	catalog := credentialOperationCatalog()
	if !catalog.Known("jira.issue.read") || catalog.Class("jira.issue.read") != "read" {
		t.Fatal("the catalogue does not know a built-in operation")
	}
	if catalog.Known("jira.admin.everything") || len(catalog.MaterialKeys("qtest", "oauth_delegated")) != 0 {
		t.Fatal("the catalogue knows an undeclared operation or class")
	}
}

// A nil Entra composition leaves the decider on NoDelegations, never a
// typed nil store.
func TestEntraDelegationsIsARealNil(t *testing.T) {
	if entraDelegations(nil) != nil || entraDelegations(&entraGovernance{}) != nil {
		t.Fatal("an absent Entra custody was boxed into the delegation reader")
	}
}

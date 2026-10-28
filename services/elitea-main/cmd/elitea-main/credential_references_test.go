package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// Flag off: no handler, so the router mounts none of the credential
// reference routes and each answers 404.
func TestCredentialReferencesAdminIsNilWithTheFlagOff(t *testing.T) {
	for _, guard := range []bool{false, true} {
		h, err := credentialReferencesAdmin(profile.LegacyAdmission(guard), profile.PolicyRevision{}, nil, nil, nil)
		if err != nil || h != nil {
			t.Fatalf("legacy(%v): handler %v, err %v; want nil, nil", guard, h, err)
		}
	}
}

func TestCredentialReferencesAdminIsBuiltWithTheFlagOn(t *testing.T) {
	revision := profile.PolicyRevision{ID: "2026-10-r1"}
	governed := profile.GovernedAdmission(revision)
	// No secret store variable is set: pgvault only, its key unwrapped.
	lookup := func(string) (string, bool) { return "", false }
	if _, err := credentialReferencesAdmin(governed, revision, onceSecretStores(lookup, nil), nil, nil); err == nil {
		t.Fatal("governed without a database built a handler")
	}
	// An undialled pool: pgxpool connects lazily.
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h, err := credentialReferencesAdmin(governed, revision, onceSecretStores(lookup, pool), pool, nil)
	if err != nil || h == nil {
		t.Fatalf("governed: handler %v, err %v", h, err)
	}
}

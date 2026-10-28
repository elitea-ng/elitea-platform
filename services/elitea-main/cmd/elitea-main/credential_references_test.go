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
		h, err := credentialReferencesAdmin(profile.LegacyAdmission(guard), nil, nil)
		if err != nil || h != nil {
			t.Fatalf("legacy(%v): handler %v, err %v; want nil, nil", guard, h, err)
		}
	}
}

func TestCredentialReferencesAdminIsBuiltWithTheFlagOn(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1"})
	if _, err := credentialReferencesAdmin(governed, nil, nil); err == nil {
		t.Fatal("governed without a database built a handler")
	}
	// An undialled pool: pgxpool connects lazily.
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h, err := credentialReferencesAdmin(governed, pool, nil)
	if err != nil || h == nil {
		t.Fatalf("governed: handler %v, err %v", h, err)
	}
}

package identityrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
)

// The Form provisioning plane matches a link by its reference alone, so it
// must never read or write a federated link: those are bound to a provider and
// issuer, and only the bound resolution may use them (issue 2558). The guard
// runs before any query, so a pool that cannot connect proves it.
func TestTheProvisioningPlaneRefusesAFederatedReference(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool}
	for _, reference := range []string{"oidc:admin-sub", "saml:admin@corp.example"} {
		_, err := repository.Provision(context.Background(), identity.ProvisionCommand{
			ProviderReference: reference, Email: "admin@corp.example",
		})
		if !errors.Is(err, identity.ErrIdentityConflict) {
			t.Fatalf("%s: err = %v, want the identity conflict before any query", reference, err)
		}
	}
	if IsFederatedProviderRef("alice") || !IsFederatedProviderRef("oidc:x") || !IsFederatedProviderRef("saml:x") {
		t.Fatal("IsFederatedProviderRef disagrees with the federated namespaces")
	}
}

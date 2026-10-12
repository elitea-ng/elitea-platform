package runtimecomposition

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVectorIntrospectionClientsParse(t *testing.T) {
	lookup := func(value string) LookupEnv {
		return func(name string) (string, bool) {
			if name == VectorIntrospectionClientsEnv {
				return value, true
			}
			return "", false
		}
	}
	clients, err := vectorIntrospectionClients(lookup(" dns:elitea-vector , spiffe://elitea.example/vector,"))
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 || clients[0] != "dns:elitea-vector" || clients[1] != "spiffe://elitea.example/vector" {
		t.Fatalf("clients = %v", clients)
	}
	if clients, err := vectorIntrospectionClients(lookup("")); err != nil || clients != nil {
		t.Fatalf("empty: %v %v", clients, err)
	}
	for _, bad := range []string{"elitea-vector", "dns:a b", "https://x"} {
		if _, err := vectorIntrospectionClients(lookup(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestVectorIntrospectionRequiresTheRuntime(t *testing.T) {
	_, err := ConfigFromEnv(func(name string) (string, bool) {
		if name == VectorIntrospectionClientsEnv {
			return "dns:elitea-vector", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("accepted introspection clients with the runtime disabled")
	}
}

type stubValidator struct{}

func (stubValidator) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{}, auth.ErrCredentialRejected
}

func TestNewVectorIntrospectionComposesOnlyWhenConfigured(t *testing.T) {
	server, err := newVectorIntrospection(Config{}, Dependencies{})
	if err != nil || server != nil {
		t.Fatalf("unconfigured: %v %v", server, err)
	}
	configured := Config{VectorIntrospectionClients: []string{"dns:elitea-vector"}}
	if _, err := newVectorIntrospection(configured, Dependencies{}); err == nil {
		t.Fatal("configured without dependencies was accepted")
	}
	if _, err := newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator: stubValidator{},
		CallbackTokenFacts:    repos.NewCallbackTokenGrants(nil),
	}); err == nil {
		t.Fatal("configured without an introspection pool was accepted")
	}
	// The control pool is not an introspection pool.
	control := &pgxpool.Pool{}
	if _, err := newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator:   stubValidator{},
		CallbackTokenFacts:      repos.NewCallbackTokenGrants(nil),
		ControlPool:             control,
		VectorIntrospectionPool: control,
	}); err == nil {
		t.Fatal("the control pool was accepted as the introspection pool")
	}
	// Only the control pool composed: introspection has none of its own.
	if _, err := newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator: stubValidator{},
		CallbackTokenFacts:    repos.NewCallbackTokenGrants(nil),
		ControlPool:           control,
	}); err == nil {
		t.Fatal("configured with only the control pool was accepted")
	}
	server, err = newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator: stubValidator{},
		CallbackTokenFacts:    repos.NewCallbackTokenGrants(nil),
		ControlPool:           control,
		// Never queried here; the claim token store only holds it.
		VectorIntrospectionPool: &pgxpool.Pool{},
	})
	if err != nil || server == nil {
		t.Fatalf("configured: %v %v", server, err)
	}
}

// The per-claim token is minted only where it can be verified: with vector
// token introspection served, and never as a typed nil otherwise.
func TestVectorClaimTokenIssuerComposesOnlyWithIntrospection(t *testing.T) {
	issuer, err := newVectorClaimTokenIssuer(Config{}, Dependencies{ControlPool: &pgxpool.Pool{}})
	if err != nil || issuer != nil {
		t.Fatalf("unconfigured: %v %v", issuer, err)
	}
	configured := Config{VectorIntrospectionClients: []string{"dns:elitea-vector"}}
	if _, err := newVectorClaimTokenIssuer(configured, Dependencies{}); err == nil {
		t.Fatal("configured without a control pool was accepted")
	}
	issuer, err = newVectorClaimTokenIssuer(configured, Dependencies{ControlPool: &pgxpool.Pool{}})
	if err != nil || issuer == nil {
		t.Fatalf("configured: %v %v", issuer, err)
	}
}

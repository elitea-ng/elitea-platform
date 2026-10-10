package runtimecomposition

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
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
	server, err = newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator: stubValidator{},
		CallbackTokenFacts:    repos.NewCallbackTokenGrants(nil),
	})
	if err != nil || server == nil {
		t.Fatalf("configured: %v %v", server, err)
	}
}

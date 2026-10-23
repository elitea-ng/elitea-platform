package runtimecomposition

import (
	"context"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
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

// With the runtime off the service has only the platform listener to be
// served on: the clients need that listener open, and are read when it is.
func TestVectorIntrospectionWithoutTheRuntimeNeedsThePlatformListener(t *testing.T) {
	_, err := ConfigFromEnv(func(name string) (string, bool) {
		if name == VectorIntrospectionClientsEnv {
			return "dns:elitea-vector", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_PLATFORM_ADDRESS") {
		t.Fatalf("accepted introspection clients with no listener to serve them: %v", err)
	}
	env := map[string]string{
		VectorIntrospectionClientsEnv:                "dns:elitea-vector",
		"ELITEA_RUNTIME_PLATFORM_ADDRESS":            ":9446",
		"ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE":      "/tls/tls.crt",
		"ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE":       "/tls/tls.key",
		"ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE": "/tls/ca.crt",
	}
	config, err := ConfigFromEnv(func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.PlatformAddress != ":9446" ||
		len(config.VectorIntrospectionClients) != 1 || config.VectorIntrospectionClients[0] != "dns:elitea-vector" {
		t.Fatalf("config %+v", config)
	}
	env[VectorIntrospectionClientsEnv] = "elitea-vector"
	if _, err := ConfigFromEnv(func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}); err == nil {
		t.Fatal("a malformed identity was accepted with the runtime off")
	}
}

// The platform listener's introspection: nothing without clients; the
// runtime's own server when the runtime is on; a callback-only server, built
// from the validator and the grants alone, when it is off.
func TestNewPlatformVectorIntrospection(t *testing.T) {
	dependencies := PlatformVectorIntrospection{
		Validator: stubValidator{}, CallbackFacts: repos.NewCallbackTokenGrants(nil),
	}
	if _, ok, err := NewPlatformVectorIntrospection(Config{}, nil, dependencies); ok || err != nil {
		t.Fatalf("unconfigured: %v %v", ok, err)
	}
	configured := Config{VectorIntrospectionClients: []string{"dns:elitea-vector"}}
	if _, _, err := NewPlatformVectorIntrospection(configured, nil, PlatformVectorIntrospection{}); err == nil {
		t.Fatal("runtime off without a validator was accepted")
	}
	if _, _, err := NewPlatformVectorIntrospection(configured, nil,
		PlatformVectorIntrospection{Validator: stubValidator{}}); err == nil {
		t.Fatal("runtime off without callback facts was accepted")
	}
	service, ok, err := NewPlatformVectorIntrospection(configured, nil, dependencies)
	if err != nil || !ok || service.Register == nil ||
		!service.Clients.Allows("dns:elitea-vector") || service.Clients.Allows("dns:elitea-worker") {
		t.Fatalf("runtime off: %+v %v %v", service, ok, err)
	}
	if _, _, err := NewPlatformVectorIntrospection(configured, &Runtime{}, dependencies); err == nil {
		t.Fatal("a runtime that composed no introspection was accepted")
	}
	runtimeServer, err := newVectorIntrospection(configured, Dependencies{
		ProjectTokenValidator:   stubValidator{},
		CallbackTokenFacts:      repos.NewCallbackTokenGrants(nil),
		ControlPool:             &pgxpool.Pool{},
		VectorIntrospectionPool: &pgxpool.Pool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, ok, err = NewPlatformVectorIntrospection(configured, &Runtime{vectorIntrospection: runtimeServer}, PlatformVectorIntrospection{})
	if err != nil || !ok || !service.Clients.Allows("dns:elitea-vector") {
		t.Fatalf("runtime on: %+v %v %v", service, ok, err)
	}
	registrar := &capturingRegistrar{}
	service.Register(registrar)
	if registrar.impl != runtimeServer {
		t.Fatal("the platform listener does not serve the runtime's own introspection server")
	}
}

type capturingRegistrar struct{ impl any }

func (r *capturingRegistrar) RegisterService(_ *grpc.ServiceDesc, impl any) { r.impl = impl }

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

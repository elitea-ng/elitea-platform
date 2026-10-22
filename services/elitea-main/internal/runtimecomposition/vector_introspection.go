package runtimecomposition

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
)

// VectorIntrospectionClientsEnv lists the client certificate identities that
// may call elitea.vector.v1.TokenIntrospectionService (ADR-0031):
// elitea-vector's, as "dns:<name>" or a spiffe:// URI. The service is served
// on the runtime control listener when the runtime is on, and on the platform
// listener (ELITEA_RUNTIME_PLATFORM_ADDRESS) whenever that is open — runtime
// on or off — behind this same list. Empty serves it nowhere.
const VectorIntrospectionClientsEnv = "ELITEA_VECTOR_INTROSPECTION_CLIENTS"

// maxVectorIntrospectionClients bounds the list; one per vector deployment.
const maxVectorIntrospectionClients = 16

// newVectorIntrospection builds the introspection service when clients are
// configured. Its claim-token reads use VectorIntrospectionPool, a small
// bounded pool of its own (ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS,
// 8). The mint stays on the control pool: it is part of the claim. It returns
// nil when no clients are configured; vectorIntrospectionOrNil turns that into
// a nil INTERFACE, never a typed nil, so the private server set leaves the
// service unregistered.
func newVectorIntrospection(config Config, dependencies Dependencies) (*vectorintrospection.Server, error) {
	if len(config.VectorIntrospectionClients) == 0 {
		return nil, nil
	}
	if dependencies.ProjectTokenValidator == nil || dependencies.CallbackTokenFacts == nil || dependencies.VectorIntrospectionPool == nil {
		return nil, errors.New(VectorIntrospectionClientsEnv +
			" is set, but the token validator, the callback token facts or the introspection pool are not composed")
	}
	// Introspection is called once per uncached token by every vector request;
	// it must not compete with lease and claim traffic for the control pool.
	if dependencies.VectorIntrospectionPool == dependencies.ControlPool {
		return nil, errors.New(VectorIntrospectionClientsEnv +
			" is set, but the introspection pool is the control pool: it needs its own")
	}
	server, err := vectorintrospection.NewServer(
		dependencies.ProjectTokenValidator,
		dependencies.CallbackTokenFacts,
		repos.NewVectorClaimTokens(dependencies.VectorIntrospectionPool),
		config.VectorIntrospectionClients,
		dependencies.Logger,
	)
	if err != nil {
		return nil, fmt.Errorf("construct vector token introspection: %w", err)
	}
	return server, nil
}

// vectorIntrospectionOrNil is server as the private server set takes it: a
// nil interface for a nil server.
func vectorIntrospectionOrNil(server *vectorintrospection.Server) vectorv1.TokenIntrospectionServiceServer {
	if server == nil {
		return nil
	}
	return server
}

// PlatformVectorIntrospection is what the platform listener's token
// introspection is built from when the runtime plane is off.
type PlatformVectorIntrospection struct {
	// Validator is the PAT validator (the form graph). Required.
	Validator vectorintrospection.TokenValidator
	// CallbackFacts reads the callback grants the facades record. Required.
	CallbackFacts vectorintrospection.CallbackFacts
	Logger        *slog.Logger
}

// NewPlatformVectorIntrospection is the platform listener's
// TokenIntrospectionService, behind the same ELITEA_VECTOR_INTROSPECTION_CLIENTS
// allowlist as the control listener's. ok is false — nothing registers — when
// the list names nobody, so a deployment that never set it is unchanged.
//
// With the runtime on (runtime non-nil) it is the runtime's own server, the
// one the control listener serves: claim tokens are verified over its
// dedicated pool, and both listeners answer every token alike. With the
// runtime off it verifies provider callback tokens only
// (vectorintrospection.NewCallbackServer): no worker claims anything without
// the runtime plane, so a worker claim token is inactive there. That is what
// lets a stack with no runtime plane (the dev compose, the E2E stack) run
// elitea-vector for DeepWiki and Inventory.
func NewPlatformVectorIntrospection(config Config, runtime *Runtime, dependencies PlatformVectorIntrospection) (runtimegrpc.PlatformService, bool, error) {
	if len(config.VectorIntrospectionClients) == 0 {
		return runtimegrpc.PlatformService{}, false, nil
	}
	if runtime != nil {
		if runtime.vectorIntrospection == nil {
			return runtimegrpc.PlatformService{}, false, errors.New(VectorIntrospectionClientsEnv +
				" is set, but the runtime composed no token introspection")
		}
		return runtime.vectorIntrospection.PlatformService(), true, nil
	}
	if dependencies.Validator == nil || dependencies.CallbackFacts == nil {
		return runtimegrpc.PlatformService{}, false, errors.New(VectorIntrospectionClientsEnv +
			" is set, but the token validator or the callback token facts are not composed")
	}
	server, err := vectorintrospection.NewCallbackServer(dependencies.Validator, dependencies.CallbackFacts,
		config.VectorIntrospectionClients, dependencies.Logger)
	if err != nil {
		return runtimegrpc.PlatformService{}, false, fmt.Errorf("construct vector token introspection: %w", err)
	}
	return server.PlatformService(), true, nil
}

// newVectorClaimTokenIssuer builds the per-claim token issuer when vector
// token introspection is served: a token nothing can verify is not minted.
// It returns a nil INTERFACE, never a typed nil, otherwise.
func newVectorClaimTokenIssuer(config Config, dependencies Dependencies) (control.VectorTokenIssuer, error) {
	if len(config.VectorIntrospectionClients) == 0 {
		return nil, nil
	}
	if dependencies.ControlPool == nil {
		return nil, errors.New(VectorIntrospectionClientsEnv + " is set, but the control pool is not composed")
	}
	issuer, err := vectorintrospection.NewClaimTokenIssuer(repos.NewVectorClaimTokens(dependencies.ControlPool))
	if err != nil {
		return nil, fmt.Errorf("construct vector claim token issuer: %w", err)
	}
	return issuer, nil
}

func vectorIntrospectionClients(lookup LookupEnv) ([]string, error) {
	raw, _ := lookup(VectorIntrospectionClientsEnv)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var clients []string
	for _, part := range strings.Split(raw, ",") {
		client := strings.TrimSpace(part)
		if client == "" {
			continue
		}
		if len(client) > 512 || strings.ContainsAny(client, "\r\n\x00 ") ||
			(!strings.HasPrefix(client, "dns:") && !strings.HasPrefix(client, "spiffe://")) {
			return nil, fmt.Errorf("%s entries are dns:<name> or spiffe:// identities", VectorIntrospectionClientsEnv)
		}
		clients = append(clients, client)
	}
	if len(clients) > maxVectorIntrospectionClients {
		return nil, errors.New(VectorIntrospectionClientsEnv + " names too many identities")
	}
	return clients, nil
}

package runtimecomposition

import (
	"errors"
	"fmt"
	"strings"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
)

// VectorIntrospectionClientsEnv lists the client certificate identities that
// may call elitea.vector.v1.TokenIntrospectionService on the runtime control
// listener (ADR-0031): elitea-vector's, as "dns:<name>" or a spiffe:// URI.
const VectorIntrospectionClientsEnv = "ELITEA_VECTOR_INTROSPECTION_CLIENTS"

// maxVectorIntrospectionClients bounds the list; one per vector deployment.
const maxVectorIntrospectionClients = 16

// newVectorIntrospection builds the introspection service when clients are
// configured. Its claim-token reads use VectorIntrospectionPool, a small
// bounded pool of its own (ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS,
// 8). The mint stays on the control pool: it is part of the claim. It returns a nil INTERFACE, never a typed nil, when they are
// not, so the private server set leaves the service unregistered.
func newVectorIntrospection(config Config, dependencies Dependencies) (vectorv1.TokenIntrospectionServiceServer, error) {
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

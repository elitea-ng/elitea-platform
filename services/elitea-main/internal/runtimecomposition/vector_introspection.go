package runtimecomposition

import (
	"errors"
	"fmt"
	"strings"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
)

// VectorIntrospectionClientsEnv lists the client certificate identities that
// may call elitea.vector.v1.TokenIntrospectionService on the runtime control
// listener (ADR-0031): elitea-vector's, as "dns:<name>" or a spiffe:// URI.
const VectorIntrospectionClientsEnv = "ELITEA_VECTOR_INTROSPECTION_CLIENTS"

// maxVectorIntrospectionClients bounds the list; one per vector deployment.
const maxVectorIntrospectionClients = 16

// newVectorIntrospection builds the introspection service when clients are
// configured. It returns a nil INTERFACE, never a typed nil, when they are
// not, so the private server set leaves the service unregistered.
func newVectorIntrospection(config Config, dependencies Dependencies) (vectorv1.TokenIntrospectionServiceServer, error) {
	if len(config.VectorIntrospectionClients) == 0 {
		return nil, nil
	}
	if dependencies.ProjectTokenValidator == nil || dependencies.CallbackTokenFacts == nil {
		return nil, errors.New(VectorIntrospectionClientsEnv +
			" is set, but the token validator or the callback token facts are not composed")
	}
	server, err := vectorintrospection.NewServer(
		dependencies.ProjectTokenValidator,
		dependencies.CallbackTokenFacts,
		config.VectorIntrospectionClients,
		dependencies.Logger,
	)
	if err != nil {
		return nil, fmt.Errorf("construct vector token introspection: %w", err)
	}
	return server, nil
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

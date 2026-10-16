package runtimecomposition

import (
	"errors"
	"fmt"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/egressaudit"
)

// EgressAuditClientsEnv lists the egress proxy's client certificate
// identities admitted to elitea.runtime.v1.EgressAuditService on the control
// listener (ADR-0034 decision 9): "dns:<name>" or spiffe:// URIs,
// comma-separated. Empty leaves the service unregistered, and the proxy's
// run summaries stay in its own log.
const EgressAuditClientsEnv = "ELITEA_EGRESS_AUDIT_CLIENTS"

const maxEgressAuditClients = 16

// newEgressAudit builds the service and its merger when clients are
// configured. The merger writes through Dependencies.AuditRecorder.
func newEgressAudit(config Config, dependencies Dependencies) (*egressaudit.Server, *app.Merger, error) {
	if len(config.EgressAuditClients) == 0 {
		return nil, nil, nil
	}
	if dependencies.AuditRecorder == nil {
		return nil, nil, errors.New(EgressAuditClientsEnv + " is set, but no audit recorder is composed")
	}
	merger := app.New(dependencies.AuditRecorder, dependencies.Logger, app.Config{})
	server, err := egressaudit.NewServer(merger, config.EgressAuditClients)
	if err != nil {
		return nil, nil, fmt.Errorf("construct the egress audit service: %w", err)
	}
	merger.Start()
	return server, merger, nil
}

func egressAuditClients(lookup LookupEnv) ([]string, error) {
	return identityList(lookup, EgressAuditClientsEnv, maxEgressAuditClients)
}

// identityList parses a comma-separated list of client certificate
// identities ("dns:<name>" or spiffe:// URIs).
func identityList(lookup LookupEnv, name string, limit int) ([]string, error) {
	raw, _ := lookup(name)
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
			return nil, fmt.Errorf("%s entries are dns:<name> or spiffe:// identities", name)
		}
		clients = append(clients, client)
	}
	if len(clients) > limit {
		return nil, errors.New(name + " names too many identities")
	}
	return clients, nil
}

// egressAuditServerOrNil keeps a nil *Server from becoming a non-nil
// interface (a typed nil would be registered and panic on the first call).
func egressAuditServerOrNil(server *egressaudit.Server) runtimev1.EgressAuditServiceServer {
	if server == nil {
		return nil
	}
	return server
}

package runtimecomposition

import (
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// newRuntimeHTTPActions composes the HTTP action path: every dial (the action
// and the OAuth2 token fetch) goes through one HTTPS-only egress guard under
// ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST; effects, rules and credentials live on
// the admission database; response artifacts use the object store when one is
// composed (without it, artifact responses are refused before any dispatch).
func newRuntimeHTTPActions(config Config, dependencies Dependencies) (*storage.RuntimeHTTPActionService, error) {
	guard, err := egress.NewHTTPActionGuard(config.HTTPActionEgressAllowlist)
	if err != nil {
		return nil, err
	}
	transport, err := storage.NewHTTPActionTransport(guard)
	if err != nil {
		return nil, fmt.Errorf("construct HTTP action transport: %w", err)
	}
	actions, err := repos.NewHTTPActionRepository(dependencies.AdmissionPool)
	if err != nil {
		return nil, fmt.Errorf("construct HTTP action effects: %w", err)
	}
	credentials, err := repos.NewHTTPActionCredentials(dependencies.AdmissionPool, dependencies.CurrentConfigurations.unsecreter, transport)
	if err != nil {
		return nil, fmt.Errorf("construct HTTP action credentials: %w", err)
	}
	var artifacts *storage.HTTPActionArtifacts
	if dependencies.ObjectStore != nil {
		source, err := repos.NewCurrentRuntimeArtifactRepository(dependencies.AdmissionPool, dependencies.ObjectStore)
		if err != nil {
			return nil, fmt.Errorf("construct HTTP action artifacts: %w", err)
		}
		if artifacts, err = storage.NewHTTPActionArtifacts(source); err != nil {
			return nil, fmt.Errorf("construct HTTP action artifacts: %w", err)
		}
	}
	service, err := storage.NewRuntimeHTTPActionService(actions, actions, actions, credentials, httpActionArtifacts(artifacts), transport, dependencies.Logger)
	if err != nil {
		return nil, fmt.Errorf("construct HTTP actions: %w", err)
	}
	return service, nil
}

// httpActionArtifacts keeps an absent artifact plane a nil interface (a typed
// nil would pass the service's nil checks and fail at call time).
func httpActionArtifacts(artifacts *storage.HTTPActionArtifacts) httpaction.Artifacts {
	if artifacts == nil {
		return nil
	}
	return artifacts
}

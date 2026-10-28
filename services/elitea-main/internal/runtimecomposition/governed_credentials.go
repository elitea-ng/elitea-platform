package runtimecomposition

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// governedAgentCredentials composes the agent admission step that resolves
// and freezes governed credential references (#1119). It is nil unless
// admissions are governed: with credential governance off neither the
// resolver nor its readers are built, and admission is unchanged.
func governedAgentCredentials(admission profile.Admission, pool *pgxpool.Pool) (*agentexecutionapp.GovernedCredentials, error) {
	if !admission.Governed() {
		return nil, nil
	}
	if pool == nil {
		return nil, errors.New("governed agent credentials need the admission pool")
	}
	// Reads only: no grant, binding or audit event is written here.
	grants := repos.NewCredentialGrants(pool, nil)
	resolver, err := configurationapp.NewGovernedReferenceResolver(grants, grants, grants, nil)
	if err != nil {
		return nil, err
	}
	toolkits, err := repos.NewCurrentToolkitsRepository(pool)
	if err != nil {
		return nil, err
	}
	return agentexecutionapp.NewGovernedCredentials(resolver, repos.NewAgentIdentityBindings(pool, nil),
		governedToolkitSource{repository: toolkits}, repos.NewAgentAdmissionReplays(pool))
}

// governedToolkitSource reads a toolkit's stored row from the run's project.
type governedToolkitSource struct {
	repository currentToolkitStore
}

func (s governedToolkitSource) StoredToolkit(ctx context.Context, projectID, toolkitID int64) (string, map[string]any, bool, error) {
	if projectID <= 0 || projectID > math.MaxInt32 || toolkitID <= 0 || toolkitID > math.MaxInt32 {
		return "", nil, false, nil
	}
	toolkit, found, err := readCurrentToolkit(ctx, s.repository, int32(projectID), int32(toolkitID))
	if err != nil || !found {
		return "", nil, false, err
	}
	settings, ok := toolkit.Settings.(map[string]any)
	if !ok || settings == nil {
		return "", nil, false, errInvalidCurrentToolkitAdapterRow
	}
	return toolkit.Type, settings, true, nil
}

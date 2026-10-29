package runtimecomposition

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// newRunStepsService builds the ReportRunSteps service over the control pool
// and, when one is composed, the object store.
func newRunStepsService(dependencies Dependencies) (*runevidence.Service, error) {
	store, err := repos.ComposeRunSteps(dependencies.ControlPool)
	if err != nil {
		return nil, fmt.Errorf("construct the run step store: %w", err)
	}
	var content runevidence.ContentStore
	if dependencies.ObjectStore != nil {
		objects, err := storage.NewRunEvidenceContent(dependencies.ObjectStore)
		if err != nil {
			return nil, err
		}
		content = objects
	}
	service, err := runevidence.NewService(store, content, dependencies.Logger)
	if err != nil {
		return nil, fmt.Errorf("construct the run step service: %w", err)
	}
	return service, nil
}

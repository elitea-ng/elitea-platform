package runtimecomposition

import (
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

const (
	currentSDKValidationWaitTimeout       = 2 * time.Minute
	currentSDKValidationPollInterval      = 100 * time.Millisecond
	currentSDKValidationBestEffortTimeout = 5 * time.Second
)

func newCurrentSDKConfigurationValidator(
	available *configurationapp.CurrentAvailableCatalog,
	candidates configurationapp.CurrentSDKValidationCandidateStore,
	bundles configurationapp.InputBundleFactory,
	jobs configurationapp.ValidationJobSubmitter,
	newID executionapp.IDGenerator,
) (configurationapp.CurrentSDKConfigurationValidator, error) {
	sdk, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		return nil, err
	}
	contracts, err := NewCurrentSDKValidationContractCatalog(available, sdk)
	if err != nil {
		return nil, err
	}
	return configurationapp.NewCurrentSDKValidationExecutionValidator(
		contracts,
		bundles,
		candidates,
		jobs,
		newID,
		nil,
		configurationapp.CurrentSDKValidationExecutionPolicy{
			WaitTimeout:       currentSDKValidationWaitTimeout,
			PollInterval:      currentSDKValidationPollInterval,
			BestEffortTimeout: currentSDKValidationBestEffortTimeout,
		},
	)
}

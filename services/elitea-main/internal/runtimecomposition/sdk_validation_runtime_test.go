package runtimecomposition

import (
	"context"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestCurrentSDKConfigurationValidatorCompositionUsesPinnedCompleteCatalog(t *testing.T) {
	available, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	validator, err := newCurrentSDKConfigurationValidator(
		available,
		currentSDKValidationCandidateStoreStub{},
		currentSDKValidationBundleFactoryStub{},
		currentSDKValidationJobSubmitterStub{},
		func() (string, error) { return "revision", nil },
	)
	if err != nil || validator == nil {
		t.Fatalf("validator=%T error=%v", validator, err)
	}
	runtime := &Runtime{configurationValidator: validator}
	if runtime.CurrentSDKConfigurationValidator() != validator || (*Runtime)(nil).CurrentSDKConfigurationValidator() != nil {
		t.Fatal("runtime did not retain the composed current SDK validator")
	}
}

type currentSDKValidationCandidateStoreStub struct{}

func (currentSDKValidationCandidateStoreStub) StageCurrentSDKValidationCandidate(
	context.Context, configurationapp.CurrentSDKValidationCandidate,
) error {
	return nil
}

func (currentSDKValidationCandidateStoreStub) ObserveCurrentSDKValidationCandidate(
	context.Context, configurationapp.CurrentSDKValidationCandidateExecution,
) (configurationapp.CurrentSDKValidationCandidateStatus, error) {
	return configurationapp.CurrentSDKValidationCandidateValid, nil
}

func (currentSDKValidationCandidateStoreStub) RequestCurrentSDKValidationCancellation(
	context.Context, configurationapp.CurrentSDKValidationCandidateExecution,
) error {
	return nil
}

func (currentSDKValidationCandidateStoreStub) CleanupCurrentSDKValidationCandidate(
	context.Context, configurationapp.CurrentSDKValidationCandidate,
) error {
	return nil
}

func (currentSDKValidationCandidateStoreStub) CleanupStaleCurrentSDKValidationCandidates(
	context.Context, configurationapp.CurrentSDKValidationCleanupRequest,
) (configurationapp.CurrentSDKValidationCleanupResult, error) {
	return configurationapp.CurrentSDKValidationCleanupResult{}, nil
}

type currentSDKValidationBundleFactoryStub struct{}

func (currentSDKValidationBundleFactoryStub) BuildValidationInput(
	context.Context, string, string, string, []byte,
) (executiondomain.InputBundle, error) {
	return executiondomain.InputBundle{}, nil
}

type currentSDKValidationJobSubmitterStub struct{}

func (currentSDKValidationJobSubmitterStub) SubmitValidation(
	context.Context, executionapp.SubmitValidationRequest,
) (executionapp.AdmissionOutcome, error) {
	return executionapp.AdmissionOutcome{AdmittedAt: time.Now(), Deadline: time.Now().Add(time.Minute)}, nil
}

package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	currentSDKValidationGeneration        uint64 = 1
	currentSDKValidationIdempotencyPrefix        = "sdk-configuration-validation:"
	maxCurrentSDKValidationIdentityBytes         = 256
	maxCurrentSDKValidationRevisionBytes         = 128
	maxCurrentSDKValidationWait                  = 5 * time.Minute
	maxCurrentSDKValidationPollInterval          = 5 * time.Second
	maxCurrentSDKValidationBestEffort            = 10 * time.Second
	MaxCurrentSDKValidationCleanupBatch   int32  = 100
)

var (
	ErrInvalidCurrentSDKValidationExecution  = errors.New("invalid current SDK validation execution")
	ErrCurrentSDKValidationExecutionFailed   = errors.New("current SDK validation execution failed")
	ErrCurrentSDKValidationExecutionTimedOut = errors.New("current SDK validation execution timed out")
	ErrCurrentSDKValidationCandidateNotFound = errors.New("current SDK validation candidate was not found")
)

// CurrentSDKValidationContract is the exact catalog/schema identity accepted
// by the deployed configuration.validate.v1 worker. Its resolver is composed
// from the checked worker manifest rather than caller settings.
type CurrentSDKValidationContract struct {
	ConfigurationType string
	CatalogRevision   string
	CatalogDigest     runtimedomain.Digest
	SchemaID          string
	SchemaRevision    string
	SchemaDigest      runtimedomain.Digest
	SettingsEntryID   string
}

func (c CurrentSDKValidationContract) Validate(configurationType string) error {
	if configurationType == "" || c.ConfigurationType != configurationType ||
		!currentSDKValidationIdentity(c.CatalogRevision) || c.CatalogDigest.IsZero() ||
		!currentSDKValidationIdentity(c.SchemaID) || !currentSDKValidationIdentity(c.SchemaRevision) ||
		c.SchemaDigest.IsZero() || !currentSDKValidationIdentity(c.SettingsEntryID) {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

type CurrentSDKValidationContractResolver interface {
	ResolveCurrentSDKValidationContract(context.Context, string) (CurrentSDKValidationContract, error)
}

// CurrentSDKValidationCandidate is the temporary tenant revision required by
// the existing output projector. ConfigurationID is deliberately absent: the
// repository must persist configuration_id NULL until the real create commits.
type CurrentSDKValidationCandidate struct {
	ProjectID             int32
	RevisionID            string
	ConfigurationType     string
	SettingsEntryID       string
	SettingsEntryVersion  string
	SettingsContentDigest runtimedomain.Digest
	InputBundleID         string
	InputBundleDigest     runtimedomain.Digest
	CatalogRevision       string
	CatalogDigest         runtimedomain.Digest
	SchemaID              string
	SchemaRevision        string
	SchemaDigest          runtimedomain.Digest
	CreatedBy             string
}

func (c CurrentSDKValidationCandidate) Validate() error {
	if c.ProjectID <= 0 || !currentSDKValidationRevision(c.RevisionID) ||
		!currentSDKValidationIdentity(c.ConfigurationType) || !currentSDKValidationIdentity(c.SettingsEntryID) ||
		!currentSDKValidationIdentity(c.SettingsEntryVersion) || c.SettingsContentDigest.IsZero() ||
		!currentSDKValidationIdentity(c.InputBundleID) || c.InputBundleDigest.IsZero() ||
		!currentSDKValidationIdentity(c.CatalogRevision) || c.CatalogDigest.IsZero() ||
		!currentSDKValidationIdentity(c.SchemaID) || !currentSDKValidationIdentity(c.SchemaRevision) ||
		c.SchemaDigest.IsZero() || !currentSDKValidationIdentity(c.CreatedBy) {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

type CurrentSDKValidationCandidateExecution struct {
	Candidate   CurrentSDKValidationCandidate
	ExecutionID string
	CommandID   string
	Generation  uint64
}

func (e CurrentSDKValidationCandidateExecution) Validate() error {
	if err := e.Candidate.Validate(); err != nil || !currentSDKValidationIdentity(e.ExecutionID) ||
		!currentSDKValidationIdentity(e.CommandID) || e.Generation != currentSDKValidationGeneration {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

type CurrentSDKValidationCandidateStatus string

const (
	CurrentSDKValidationCandidatePending   CurrentSDKValidationCandidateStatus = "PENDING"
	CurrentSDKValidationCandidateValid     CurrentSDKValidationCandidateStatus = "VALID"
	CurrentSDKValidationCandidateInvalid   CurrentSDKValidationCandidateStatus = "INVALID"
	CurrentSDKValidationCandidateFailed    CurrentSDKValidationCandidateStatus = "FAILED"
	CurrentSDKValidationCandidateCancelled CurrentSDKValidationCandidateStatus = "CANCELLED"
)

// CurrentSDKValidationCleanupRequest bounds one tenant-local janitor pass.
// OlderThan must be an operator-selected retention cutoff rather than the
// current request deadline.
type CurrentSDKValidationCleanupRequest struct {
	ProjectID int32
	OlderThan time.Time
	Limit     int32
}

func (r CurrentSDKValidationCleanupRequest) Validate() error {
	if r.ProjectID <= 0 || r.OlderThan.IsZero() || r.Limit <= 0 || r.Limit > MaxCurrentSDKValidationCleanupBatch {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

// CurrentSDKValidationCleanupResult separates safe deletion from observation.
// Unreferenced candidates are only counted: without a durable admission-
// abandoned marker an old staged row cannot be distinguished from an
// admission that is still committing in another transaction.
type CurrentSDKValidationCleanupResult struct {
	TerminalDeleted      int32
	UnreferencedObserved int32
}

// CurrentSDKValidationCandidateStore owns short database operations only.
// Observe must finish its read transaction before returning; the application
// waits between calls and never holds a transaction while a worker executes.
type CurrentSDKValidationCandidateStore interface {
	StageCurrentSDKValidationCandidate(context.Context, CurrentSDKValidationCandidate) error
	ObserveCurrentSDKValidationCandidate(context.Context, CurrentSDKValidationCandidateExecution) (CurrentSDKValidationCandidateStatus, error)
	RequestCurrentSDKValidationCancellation(context.Context, CurrentSDKValidationCandidateExecution) error
	CleanupCurrentSDKValidationCandidate(context.Context, CurrentSDKValidationCandidate) error
}

// CurrentSDKValidationCandidateJanitor is separate from the request-path store
// so validation consumers do not depend on an operational maintenance method.
type CurrentSDKValidationCandidateJanitor interface {
	CleanupStaleCurrentSDKValidationCandidates(context.Context, CurrentSDKValidationCleanupRequest) (CurrentSDKValidationCleanupResult, error)
}

type CurrentSDKValidationPollWaiter interface {
	WaitCurrentSDKValidationPoll(context.Context) error
}

type CurrentSDKValidationExecutionPolicy struct {
	WaitTimeout       time.Duration
	PollInterval      time.Duration
	BestEffortTimeout time.Duration
}

func (p CurrentSDKValidationExecutionPolicy) validate() error {
	if p.WaitTimeout <= 0 || p.WaitTimeout > maxCurrentSDKValidationWait ||
		p.PollInterval <= 0 || p.PollInterval > maxCurrentSDKValidationPollInterval || p.PollInterval > p.WaitTimeout ||
		p.BestEffortTimeout <= 0 || p.BestEffortTimeout > maxCurrentSDKValidationBestEffort {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

// CurrentSDKValidationExecutionValidator adapts synchronous SDK validation to
// the existing durable configuration.validate.v1 execution. Settings bytes are
// admitted only through the immutable input bundle; command-bus dispatch
// remains reference-only through the existing execution publisher.
type CurrentSDKValidationExecutionValidator struct {
	contracts  CurrentSDKValidationContractResolver
	bundles    InputBundleFactory
	candidates CurrentSDKValidationCandidateStore
	jobs       ValidationJobSubmitter
	newID      executionapp.IDGenerator
	waiter     CurrentSDKValidationPollWaiter
	policy     CurrentSDKValidationExecutionPolicy
}

func NewCurrentSDKValidationExecutionValidator(
	contracts CurrentSDKValidationContractResolver,
	bundles InputBundleFactory,
	candidates CurrentSDKValidationCandidateStore,
	jobs ValidationJobSubmitter,
	newID executionapp.IDGenerator,
	waiter CurrentSDKValidationPollWaiter,
	policy CurrentSDKValidationExecutionPolicy,
) (*CurrentSDKValidationExecutionValidator, error) {
	if contracts == nil || bundles == nil || candidates == nil || jobs == nil || newID == nil {
		return nil, ErrInvalidCurrentSDKValidationExecution
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if waiter == nil {
		waiter = currentSDKValidationTimerWaiter{interval: policy.PollInterval}
	}
	return &CurrentSDKValidationExecutionValidator{
		contracts: contracts, bundles: bundles, candidates: candidates, jobs: jobs,
		newID: newID, waiter: waiter, policy: policy,
	}, nil
}

func (v *CurrentSDKValidationExecutionValidator) ValidateCurrentSDKConfiguration(
	ctx context.Context,
	request CurrentSDKConfigurationValidationRequest,
) error {
	if ctx == nil || v == nil || request.ProjectID <= 0 || request.AuthorID <= 0 ||
		!currentSDKValidationIdentity(request.Type) || request.Settings == nil {
		return ErrInvalidCurrentSDKValidationExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	settings, err := json.Marshal(request.Settings)
	if err != nil || validateSettingsJSON(settings) != nil {
		return ErrInvalidCurrentSDKValidationExecution
	}
	contract, err := v.contracts.ResolveCurrentSDKValidationContract(ctx, request.Type)
	if err != nil {
		return fmt.Errorf("resolve current SDK validation contract: %w", err)
	}
	if err := contract.Validate(request.Type); err != nil {
		return err
	}
	revisionID, err := v.newID()
	if err != nil {
		return fmt.Errorf("generate current SDK validation revision: %w", err)
	}
	if !currentSDKValidationRevision(revisionID) {
		return ErrInvalidCurrentSDKValidationExecution
	}

	bundle, err := v.bundles.BuildValidationInput(
		ctx,
		revisionID,
		contract.SettingsEntryID,
		revisionID,
		append([]byte(nil), settings...),
	)
	if err != nil {
		return fmt.Errorf("build current SDK validation input: %w", err)
	}
	if err := validateCurrentSDKValidationBundle(bundle, revisionID, contract.SettingsEntryID, settings); err != nil {
		return err
	}

	projectID := strconv.FormatInt(int64(request.ProjectID), 10)
	authorID := strconv.FormatInt(int64(request.AuthorID), 10)
	candidate := CurrentSDKValidationCandidate{
		ProjectID:             request.ProjectID,
		RevisionID:            revisionID,
		ConfigurationType:     request.Type,
		SettingsEntryID:       contract.SettingsEntryID,
		SettingsEntryVersion:  revisionID,
		SettingsContentDigest: runtimedomain.SHA256(settings),
		InputBundleID:         bundle.ID,
		InputBundleDigest:     bundle.Digest,
		CatalogRevision:       contract.CatalogRevision,
		CatalogDigest:         contract.CatalogDigest,
		SchemaID:              contract.SchemaID,
		SchemaRevision:        contract.SchemaRevision,
		SchemaDigest:          contract.SchemaDigest,
		CreatedBy:             authorID,
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := v.candidates.StageCurrentSDKValidationCandidate(ctx, candidate); err != nil {
		return fmt.Errorf("stage current SDK validation candidate: %w", err)
	}

	command := configurationdomain.ValidationCommand{
		ConfigurationRevisionID: revisionID,
		ConfigurationType:       request.Type,
		CatalogRevision:         contract.CatalogRevision,
		CatalogDigest:           contract.CatalogDigest,
		SchemaID:                contract.SchemaID,
		SchemaRevision:          contract.SchemaRevision,
		SchemaDigest:            contract.SchemaDigest,
		SettingsEntryID:         contract.SettingsEntryID,
	}
	outcome, err := v.jobs.SubmitValidation(ctx, executionapp.SubmitValidationRequest{
		Identity: executionapp.AdmissionIdentity{
			// The current platform has project-local schemas and no separate
			// tenant identifier, matching public validation and index admission.
			TenantID:            projectID,
			ResourceProjectID:   projectID,
			ProjectionProjectID: projectID,
			ActorID:             authorID,
		},
		IdempotencyKey: currentSDKValidationIdempotencyPrefix + revisionID,
		InputBundle:    bundle,
		Command:        command,
	})
	if err != nil {
		return fmt.Errorf("submit current SDK validation execution: %w", err)
	}
	defer v.bestEffortCleanup(ctx, candidate)
	execution := CurrentSDKValidationCandidateExecution{
		Candidate: candidate, ExecutionID: outcome.ExecutionID,
		CommandID: outcome.CommandID, Generation: currentSDKValidationGeneration,
	}
	if err := execution.Validate(); err != nil || outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return ErrInvalidCurrentSDKValidationExecution
	}

	waitCtx, stopWaiting := context.WithTimeout(ctx, v.policy.WaitTimeout)
	defer stopWaiting()
	for {
		status, observeErr := v.candidates.ObserveCurrentSDKValidationCandidate(waitCtx, execution)
		if observeErr != nil {
			v.bestEffortCancel(ctx, execution)
			if waitErr := waitCtx.Err(); waitErr != nil {
				return v.waitError(ctx, waitErr)
			}
			return fmt.Errorf("observe current SDK validation execution: %w", observeErr)
		}
		switch status {
		case CurrentSDKValidationCandidateValid:
			return nil
		case CurrentSDKValidationCandidateInvalid:
			return ErrCurrentSDKConfigurationRejected
		case CurrentSDKValidationCandidateFailed, CurrentSDKValidationCandidateCancelled:
			return ErrCurrentSDKValidationExecutionFailed
		case CurrentSDKValidationCandidatePending:
		default:
			v.bestEffortCancel(ctx, execution)
			return ErrInvalidCurrentSDKValidationExecution
		}
		if err := v.waiter.WaitCurrentSDKValidationPoll(waitCtx); err != nil {
			v.bestEffortCancel(ctx, execution)
			return v.waitError(ctx, err)
		}
	}
}

func (v *CurrentSDKValidationExecutionValidator) waitError(parent context.Context, waitErr error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return ErrCurrentSDKValidationExecutionTimedOut
	}
	return waitErr
}

func (v *CurrentSDKValidationExecutionValidator) bestEffortCancel(
	ctx context.Context,
	execution CurrentSDKValidationCandidateExecution,
) {
	bestEffortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.policy.BestEffortTimeout)
	defer cancel()
	_ = v.candidates.RequestCurrentSDKValidationCancellation(bestEffortCtx, execution)
}

func (v *CurrentSDKValidationExecutionValidator) bestEffortCleanup(
	ctx context.Context,
	candidate CurrentSDKValidationCandidate,
) {
	bestEffortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.policy.BestEffortTimeout)
	defer cancel()
	_ = v.candidates.CleanupCurrentSDKValidationCandidate(bestEffortCtx, candidate)
}

func validateCurrentSDKValidationBundle(
	bundle executiondomain.InputBundle,
	revisionID string,
	settingsEntryID string,
	settings []byte,
) error {
	if err := bundle.Validate(); err != nil || bundle.Version != revisionID || len(bundle.Entries) != 1 {
		return ErrInvalidCurrentSDKValidationExecution
	}
	entry := bundle.Entries[0]
	settingsDigest := runtimedomain.SHA256(settings)
	if entry.ID != settingsEntryID || entry.Version != revisionID ||
		entry.SemanticRole != "configuration.settings" || entry.MediaType != executiondomain.SettingsJSONMediaType ||
		entry.ContentDigest != settingsDigest || !bytes.Equal(entry.Content, settings) {
		return ErrInvalidCurrentSDKValidationExecution
	}
	return nil
}

func currentSDKValidationIdentity(value string) bool {
	return value != "" && len(value) <= maxCurrentSDKValidationIdentityBytes && !strings.ContainsRune(value, '\x00')
}

func currentSDKValidationRevision(value string) bool {
	return value != "" && len(value) <= maxCurrentSDKValidationRevisionBytes && !strings.ContainsRune(value, '\x00')
}

type currentSDKValidationTimerWaiter struct {
	interval time.Duration
}

func (w currentSDKValidationTimerWaiter) WaitCurrentSDKValidationPoll(ctx context.Context) error {
	timer := time.NewTimer(w.interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ CurrentSDKConfigurationValidator = (*CurrentSDKValidationExecutionValidator)(nil)

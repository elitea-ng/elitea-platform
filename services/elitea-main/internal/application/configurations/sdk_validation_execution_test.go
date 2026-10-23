package configurations

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestCurrentSDKValidationExecutionUsesActualBundleAndWaitsForProjectionAndSettlement(t *testing.T) {
	calls := make([]string, 0, 6)
	contracts := &sdkExecutionContractStub{contract: sdkExecutionContract("github")}
	bundles := &sdkExecutionBundleStub{calls: &calls}
	candidates := &sdkExecutionCandidateStoreStub{
		calls:        &calls,
		observations: []CurrentSDKValidationCandidateStatus{CurrentSDKValidationCandidatePending, CurrentSDKValidationCandidateValid},
	}
	jobs := &sdkExecutionJobStub{trace: &calls, outcome: sdkExecutionOutcome()}
	waiter := &sdkExecutionWaiterStub{calls: &calls}
	validator := newSDKExecutionValidator(t, contracts, bundles, candidates, jobs, waiter)

	settings := map[string]any{
		"base_url":     "https://api.github.com",
		"access_token": "expanded-secret",
	}
	err := validator.ValidateCurrentSDKConfiguration(context.Background(), CurrentSDKConfigurationValidationRequest{
		ProjectID: 7, AuthorID: 13, Type: "github", Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{"bundle", "stage", "submit", "observe", "wait", "observe", "cleanup"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
	if candidates.staged.InputBundleID != "actual-input-bundle" ||
		candidates.staged.InputBundleID != jobs.request.InputBundle.ID ||
		candidates.staged.InputBundleDigest != jobs.request.InputBundle.Digest {
		t.Fatalf("candidate was not bound to actual admitted bundle: candidate=%+v bundle=%+v", candidates.staged, jobs.request.InputBundle)
	}
	if candidates.staged.RevisionID != "candidate-revision" || candidates.staged.SettingsEntryVersion != "candidate-revision" ||
		candidates.staged.CreatedBy != "13" || candidates.staged.ProjectID != 7 {
		t.Fatalf("candidate identity = %+v", candidates.staged)
	}
	if bundles.revisionID != candidates.staged.RevisionID || bundles.entryID != contracts.contract.SettingsEntryID ||
		bundles.entryVersion != candidates.staged.SettingsEntryVersion {
		t.Fatalf("bundle binding was not exact: %+v", bundles)
	}
	if got := string(bundles.settings); got != `{"access_token":"expanded-secret","base_url":"https://api.github.com"}` {
		t.Fatalf("settings bytes = %q", got)
	}
	if jobs.request.Identity.TenantID != "7" || jobs.request.Identity.ResourceProjectID != "7" ||
		jobs.request.Identity.ProjectionProjectID != "7" || jobs.request.Identity.ActorID != "13" {
		t.Fatalf("admission identity = %+v", jobs.request.Identity)
	}
	if jobs.request.Command.ConfigurationRevisionID != candidates.staged.RevisionID ||
		jobs.request.Command.CatalogDigest != contracts.contract.CatalogDigest ||
		jobs.request.Command.SchemaDigest != contracts.contract.SchemaDigest {
		t.Fatalf("admitted command = %+v", jobs.request.Command)
	}
	if candidates.cancelCalls != 0 || candidates.cleanupCalls != 1 {
		t.Fatalf("cancel=%d cleanup=%d", candidates.cancelCalls, candidates.cleanupCalls)
	}
}

func TestCurrentSDKValidationExecutionMapsOnlyDurableBusinessInvalidToRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		status CurrentSDKValidationCandidateStatus
		want   error
	}{
		{name: "business invalid", status: CurrentSDKValidationCandidateInvalid, want: ErrCurrentSDKConfigurationRejected},
		{name: "worker failed", status: CurrentSDKValidationCandidateFailed, want: ErrCurrentSDKValidationExecutionFailed},
		{name: "worker cancelled", status: CurrentSDKValidationCandidateCancelled, want: ErrCurrentSDKValidationExecutionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidates := &sdkExecutionCandidateStoreStub{observations: []CurrentSDKValidationCandidateStatus{test.status}}
			validator := newSDKExecutionValidator(
				t,
				&sdkExecutionContractStub{contract: sdkExecutionContract("github")},
				&sdkExecutionBundleStub{},
				candidates,
				&sdkExecutionJobStub{outcome: sdkExecutionOutcome()},
				&sdkExecutionWaiterStub{},
			)
			err := validator.ValidateCurrentSDKConfiguration(context.Background(), CurrentSDKConfigurationValidationRequest{
				ProjectID: 7, AuthorID: 13, Type: "github", Settings: map[string]any{"base_url": "https://api.github.com"},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if candidates.cleanupCalls != 1 || candidates.cancelCalls != 0 {
				t.Fatalf("cancel=%d cleanup=%d", candidates.cancelCalls, candidates.cleanupCalls)
			}
		})
	}
}

func TestCurrentSDKValidationExecutionCancellationRequestsDurableCancellationBestEffort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	candidates := &sdkExecutionCandidateStoreStub{observations: []CurrentSDKValidationCandidateStatus{CurrentSDKValidationCandidatePending}}
	waiter := &sdkExecutionWaiterStub{wait: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}}
	validator := newSDKExecutionValidator(
		t,
		&sdkExecutionContractStub{contract: sdkExecutionContract("github")},
		&sdkExecutionBundleStub{},
		candidates,
		&sdkExecutionJobStub{outcome: sdkExecutionOutcome()},
		waiter,
	)

	err := validator.ValidateCurrentSDKConfiguration(ctx, CurrentSDKConfigurationValidationRequest{
		ProjectID: 7, AuthorID: 13, Type: "github", Settings: map[string]any{"base_url": "https://api.github.com"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if candidates.cancelCalls != 1 || candidates.cleanupCalls != 1 || candidates.bestEffortContextCancelled {
		t.Fatalf("cancel=%d cleanup=%d detached_cancelled=%v", candidates.cancelCalls, candidates.cleanupCalls, candidates.bestEffortContextCancelled)
	}
	if candidates.cancelled.ExecutionID != "execution-1" || candidates.cancelled.Candidate.InputBundleID != "actual-input-bundle" {
		t.Fatalf("cancellation binding = %+v", candidates.cancelled)
	}
}

func TestCurrentSDKValidationExecutionTimeoutIsTypedAndCancels(t *testing.T) {
	candidates := &sdkExecutionCandidateStoreStub{observations: []CurrentSDKValidationCandidateStatus{CurrentSDKValidationCandidatePending}}
	validator := newSDKExecutionValidator(
		t,
		&sdkExecutionContractStub{contract: sdkExecutionContract("github")},
		&sdkExecutionBundleStub{},
		candidates,
		&sdkExecutionJobStub{outcome: sdkExecutionOutcome()},
		&sdkExecutionWaiterStub{wait: func(context.Context) error { return context.DeadlineExceeded }},
	)
	err := validator.ValidateCurrentSDKConfiguration(context.Background(), CurrentSDKConfigurationValidationRequest{
		ProjectID: 7, AuthorID: 13, Type: "github", Settings: map[string]any{"base_url": "https://api.github.com"},
	})
	if !errors.Is(err, ErrCurrentSDKValidationExecutionTimedOut) || candidates.cancelCalls != 1 || candidates.cleanupCalls != 1 {
		t.Fatalf("error=%v cancel=%d cleanup=%d", err, candidates.cancelCalls, candidates.cleanupCalls)
	}
}

func TestCurrentSDKValidationExecutionRejectsInvalidSettingsBeforeDurableEffects(t *testing.T) {
	contracts := &sdkExecutionContractStub{contract: sdkExecutionContract("github")}
	bundles := &sdkExecutionBundleStub{}
	candidates := &sdkExecutionCandidateStoreStub{}
	jobs := &sdkExecutionJobStub{outcome: sdkExecutionOutcome()}
	validator := newSDKExecutionValidator(t, contracts, bundles, candidates, jobs, &sdkExecutionWaiterStub{})
	err := validator.ValidateCurrentSDKConfiguration(context.Background(), CurrentSDKConfigurationValidationRequest{
		ProjectID: 7, AuthorID: 13, Type: "github", Settings: map[string]any{"value": math.NaN()},
	})
	if !errors.Is(err, ErrInvalidCurrentSDKValidationExecution) {
		t.Fatalf("error = %v", err)
	}
	if contracts.calls != 0 || bundles.callsCount != 0 || candidates.stageCalls != 0 || jobs.callCount != 0 {
		t.Fatalf("invalid settings reached dependencies: contract=%d bundle=%d stage=%d submit=%d", contracts.calls, bundles.callsCount, candidates.stageCalls, jobs.callCount)
	}
}

func TestCurrentSDKValidationCleanupRequestBoundsTenantAgeAndBatch(t *testing.T) {
	valid := CurrentSDKValidationCleanupRequest{
		ProjectID: 7,
		OlderThan: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Limit:     MaxCurrentSDKValidationCleanupBatch,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	for _, request := range []CurrentSDKValidationCleanupRequest{
		{ProjectID: 0, OlderThan: valid.OlderThan, Limit: 1},
		{ProjectID: 7, Limit: 1},
		{ProjectID: 7, OlderThan: valid.OlderThan, Limit: 0},
		{ProjectID: 7, OlderThan: valid.OlderThan, Limit: MaxCurrentSDKValidationCleanupBatch + 1},
	} {
		if err := request.Validate(); !errors.Is(err, ErrInvalidCurrentSDKValidationExecution) {
			t.Fatalf("request %+v error = %v", request, err)
		}
	}
}

func newSDKExecutionValidator(
	t *testing.T,
	contracts CurrentSDKValidationContractResolver,
	bundles InputBundleFactory,
	candidates CurrentSDKValidationCandidateStore,
	jobs ValidationJobSubmitter,
	waiter CurrentSDKValidationPollWaiter,
) *CurrentSDKValidationExecutionValidator {
	t.Helper()
	validator, err := NewCurrentSDKValidationExecutionValidator(
		contracts,
		bundles,
		candidates,
		jobs,
		func() (string, error) { return "candidate-revision", nil },
		waiter,
		CurrentSDKValidationExecutionPolicy{
			WaitTimeout: time.Minute, PollInterval: time.Millisecond, BestEffortTimeout: time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func sdkExecutionContract(configurationType string) CurrentSDKValidationContract {
	return CurrentSDKValidationContract{
		ConfigurationType: configurationType,
		CatalogRevision:   "sdk-catalog-v1",
		CatalogDigest:     runtimedomain.SHA256([]byte("catalog")),
		SchemaID:          "elitea.configuration." + configurationType,
		SchemaRevision:    "schema-v1",
		SchemaDigest:      runtimedomain.SHA256([]byte("schema")),
		SettingsEntryID:   "settings",
	}
}

func sdkExecutionOutcome() executionapp.AdmissionOutcome {
	admitted := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	return executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admitted, Deadline: admitted.Add(time.Minute),
	}
}

type sdkExecutionContractStub struct {
	contract CurrentSDKValidationContract
	err      error
	calls    int
}

func (s *sdkExecutionContractStub) ResolveCurrentSDKValidationContract(
	_ context.Context,
	_ string,
) (CurrentSDKValidationContract, error) {
	s.calls++
	return s.contract, s.err
}

type sdkExecutionBundleStub struct {
	calls        *[]string
	callsCount   int
	revisionID   string
	entryID      string
	entryVersion string
	settings     []byte
}

func (s *sdkExecutionBundleStub) BuildValidationInput(
	_ context.Context,
	revisionID string,
	entryID string,
	entryVersion string,
	settings []byte,
) (executiondomain.InputBundle, error) {
	s.callsCount++
	if s.calls != nil {
		*s.calls = append(*s.calls, "bundle")
	}
	s.revisionID = revisionID
	s.entryID = entryID
	s.entryVersion = entryVersion
	s.settings = append([]byte(nil), settings...)
	manifest := []byte("deterministic validation manifest")
	content := append([]byte(nil), settings...)
	return executiondomain.InputBundle{
		ID: "actual-input-bundle", Version: revisionID,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest), Manifest: manifest,
		Entries: []executiondomain.InputEntry{{
			ID: entryID, Version: entryVersion, SemanticRole: "configuration.settings",
			ContentID: "content-1", MediaType: executiondomain.SettingsJSONMediaType,
			Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
			ContentDigest: runtimedomain.SHA256(content), ContentLength: int64(len(content)), Content: content,
		}},
	}, nil
}

type sdkExecutionCandidateStoreStub struct {
	calls                      *[]string
	observations               []CurrentSDKValidationCandidateStatus
	observeErr                 error
	staged                     CurrentSDKValidationCandidate
	cancelled                  CurrentSDKValidationCandidateExecution
	stageCalls                 int
	cancelCalls                int
	cleanupCalls               int
	bestEffortContextCancelled bool
}

func (s *sdkExecutionCandidateStoreStub) StageCurrentSDKValidationCandidate(
	_ context.Context,
	candidate CurrentSDKValidationCandidate,
) error {
	s.stageCalls++
	s.staged = candidate
	if s.calls != nil {
		*s.calls = append(*s.calls, "stage")
	}
	return nil
}

func (s *sdkExecutionCandidateStoreStub) ObserveCurrentSDKValidationCandidate(
	_ context.Context,
	_ CurrentSDKValidationCandidateExecution,
) (CurrentSDKValidationCandidateStatus, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "observe")
	}
	if s.observeErr != nil {
		return "", s.observeErr
	}
	if len(s.observations) == 0 {
		return "", errors.New("unexpected observation")
	}
	status := s.observations[0]
	s.observations = s.observations[1:]
	return status, nil
}

func (s *sdkExecutionCandidateStoreStub) RequestCurrentSDKValidationCancellation(
	ctx context.Context,
	execution CurrentSDKValidationCandidateExecution,
) error {
	s.cancelCalls++
	s.cancelled = execution
	s.bestEffortContextCancelled = s.bestEffortContextCancelled || ctx.Err() != nil
	if s.calls != nil {
		*s.calls = append(*s.calls, "cancel")
	}
	return nil
}

func (s *sdkExecutionCandidateStoreStub) CleanupCurrentSDKValidationCandidate(
	ctx context.Context,
	_ CurrentSDKValidationCandidate,
) error {
	s.cleanupCalls++
	s.bestEffortContextCancelled = s.bestEffortContextCancelled || ctx.Err() != nil
	if s.calls != nil {
		*s.calls = append(*s.calls, "cleanup")
	}
	return nil
}

type sdkExecutionJobStub struct {
	trace     *[]string
	request   executionapp.SubmitValidationRequest
	outcome   executionapp.AdmissionOutcome
	err       error
	callCount int
}

func (s *sdkExecutionJobStub) SubmitValidation(
	_ context.Context,
	request executionapp.SubmitValidationRequest,
) (executionapp.AdmissionOutcome, error) {
	s.callCount++
	if s.trace != nil {
		*s.trace = append(*s.trace, "submit")
	}
	s.request = request
	return s.outcome, s.err
}

type sdkExecutionWaiterStub struct {
	calls *[]string
	wait  func(context.Context) error
}

func (s *sdkExecutionWaiterStub) WaitCurrentSDKValidationPoll(ctx context.Context) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "wait")
	}
	if s.wait != nil {
		return s.wait(ctx)
	}
	return nil
}

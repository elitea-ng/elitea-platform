package indexschedule

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

type scheduledToolkitReaderStub struct {
	toolkit indexingapp.CurrentToolkitSnapshot
	found   bool
	err     error
	userID  int32
	calls   int
}

func (stub *scheduledToolkitReaderStub) GetCurrentToolkit(
	_ context.Context,
	_ int32,
	userID int32,
	_ int32,
) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	stub.calls++
	stub.userID = userID
	return stub.toolkit, stub.found, stub.err
}

type scheduledSettingsStub struct {
	calls  []configurationapp.CurrentToolkitSettingsRequest
	result map[string]any
	err    error
}

func (stub *scheduledSettingsStub) Resolve(
	_ context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	stub.calls = append(stub.calls, request)
	return stub.result, stub.err
}

type scheduledSystemIdentityStub struct {
	projectID int64
	err       error
	calls     int
}

func (stub *scheduledSystemIdentityStub) CheckProjectSystemIdentity(
	_ context.Context,
	projectID int64,
) error {
	stub.calls++
	stub.projectID = projectID
	return stub.err
}

type scheduledIndexInspectorStub struct {
	index   ScheduledIndex
	found   bool
	err     error
	user    int64
	toolkit indexingapp.CurrentToolkitSnapshot
	calls   int
}

func (stub *scheduledIndexInspectorStub) InspectScheduledIndex(
	_ context.Context,
	_ Candidate,
	userID int64,
	toolkit indexingapp.CurrentToolkitSnapshot,
) (ScheduledIndex, bool, error) {
	stub.calls++
	stub.user = userID
	stub.toolkit = toolkit
	return stub.index, stub.found, stub.err
}

type scheduledInputResolverStub struct {
	request indexingapp.StartRequest
	toolkit indexingapp.CurrentToolkitSnapshot
	inputs  indexingapp.AuthoritativeInputs
	err     error
	calls   int
}

func (stub *scheduledInputResolverStub) ResolveScheduled(
	_ context.Context,
	request indexingapp.StartRequest,
	toolkit indexingapp.CurrentToolkitSnapshot,
) (indexingapp.AuthoritativeInputs, error) {
	stub.calls++
	stub.request = request.Clone()
	stub.toolkit = toolkit
	return stub.inputs.Clone(), stub.err
}

type scheduledStarterStub struct {
	request indexingapp.ScheduledStartRequest
	outcome indexingapp.StartOutcome
	err     error
	calls   int
}

func (stub *scheduledStarterStub) StartScheduledIndexData(
	_ context.Context,
	request indexingapp.ScheduledStartRequest,
) (indexingapp.StartOutcome, error) {
	stub.calls++
	stub.request = request
	return stub.outcome, stub.err
}

func TestCurrentExecutorPreservesPersonalCredentialAndAttributionRules(t *testing.T) {
	private := true
	candidate := scheduledExecutorCandidate(17)
	candidate.Schedule.Credentials = &Credentials{
		Private:     &private,
		EliteaTitle: "personal-github",
	}
	toolkits := &scheduledToolkitReaderStub{
		found: true,
		toolkit: indexingapp.CurrentToolkitSnapshot{
			ID: 7, Type: "github", Name: "repository",
			Settings: map[string]any{
				"github_configuration": map[string]any{
					"elitea_title": "project-github",
					"private":      false,
				},
				"pgvector_configuration": map[string]any{
					"elitea_title": "project-vectorstore",
					"private":      false,
				},
			},
		},
	}
	settings := &scheduledSettingsStub{result: map[string]any{
		"github_configuration": map[string]any{
			"token": "transient-plaintext-canary",
		},
	}}
	system := &scheduledSystemIdentityStub{}
	inspector := &scheduledIndexInspectorStub{
		found: true,
		index: ScheduledIndex{
			State: "COMPLETED",
			Configuration: map[string]any{
				"index_name": "docs",
				"reindex":    true,
			},
		},
	}
	inputs := &scheduledInputResolverStub{
		inputs: validScheduledInputs(),
	}
	start := &scheduledStarterStub{
		outcome: indexingapp.StartOutcome{
			TaskID:  "execution-1",
			Created: true,
		},
	}
	executor := newCurrentExecutorForTest(
		t,
		toolkits,
		settings,
		system,
		inspector,
		inputs,
		start,
	)

	outcome, err := executor.ExecuteScheduled(
		context.Background(),
		candidate,
		time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC),
		"index-schedule-v1:stable",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted ||
		toolkits.userID != 17 ||
		inspector.user != 17 ||
		system.projectID != 42 {
		t.Fatalf(
			"outcome=%+v toolkit user=%d inspect user=%d system project=%d",
			outcome,
			toolkits.userID,
			inspector.user,
			system.projectID,
		)
	}
	if len(settings.calls) != 1 ||
		settings.calls[0].Mode != configurationapp.CurrentToolkitSettingsClaimMode ||
		settings.calls[0].UserID != 17 {
		t.Fatalf("preflight calls=%+v", settings.calls)
	}
	credential := settings.calls[0].Settings["github_configuration"].(map[string]any)
	if credential["elitea_title"] != "personal-github" ||
		credential["private"] != true {
		t.Fatalf("credential override=%#v", credential)
	}
	frozenCredential := inputs.toolkit.Settings["github_configuration"].(map[string]any)
	if frozenCredential["elitea_title"] != "personal-github" ||
		frozenCredential["private"] != true {
		t.Fatalf("frozen credential reference=%#v", frozenCredential)
	}
	encodedToolkit, _ := json.Marshal(inputs.toolkit.Settings)
	if string(encodedToolkit) == "" ||
		containsBytes(encodedToolkit, []byte("transient-plaintext-canary")) {
		t.Fatalf("transient preflight escaped into frozen settings: %s", encodedToolkit)
	}
	if inputs.request.ActorUserID != 17 ||
		string(inputs.request.ToolParameters) != `{"index_name":"docs","reindex":true}` {
		t.Fatalf("scheduled input request=%+v", inputs.request)
	}
	if start.request.AttributionActorUserID != 17 ||
		start.request.IdempotencyKey != "index-schedule-v1:stable" ||
		start.request.CorrelationID != "index-schedule-v1:stable" {
		t.Fatalf("scheduled start=%+v", start.request)
	}
}

func TestCurrentExecutorAcceptsNullableCredentialPrivateLikeCurrentRuntime(t *testing.T) {
	candidate := scheduledExecutorCandidate(17)
	candidate.Schedule.Credentials = &Credentials{
		EliteaTitle: "public-github",
	}
	executor, dependencies := validCurrentExecutor(t, candidate)

	outcome, err := executor.ExecuteScheduled(
		context.Background(),
		candidate,
		time.Now(),
		"index-schedule-v1:nullable-private",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted {
		t.Fatalf("outcome=%+v", outcome)
	}
	reference := dependencies.settings.calls[0].
		Settings["github_configuration"].(map[string]any)
	if reference["elitea_title"] != "public-github" ||
		reference["private"] != nil {
		t.Fatalf("reference=%#v", reference)
	}
}

func TestCurrentExecutorUsesCreatorForTeamScheduleWithoutCredentialOverride(t *testing.T) {
	candidate := scheduledExecutorCandidate(-1)
	executor, dependencies := validCurrentExecutor(t, candidate)
	stored := dependencies.toolkits.toolkit.Settings["github_configuration"]

	outcome, err := executor.ExecuteScheduled(
		context.Background(),
		candidate,
		time.Now(),
		"index-schedule-v1:team",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted ||
		dependencies.toolkits.userID != 99 ||
		dependencies.inspector.user != 99 {
		t.Fatalf(
			"outcome=%+v toolkit user=%d inspect user=%d",
			outcome,
			dependencies.toolkits.userID,
			dependencies.inspector.user,
		)
	}
	if !reflect.DeepEqual(
		dependencies.settings.calls[0].Settings["github_configuration"],
		stored,
	) {
		t.Fatal("team schedule changed the project credential")
	}
}

func TestCurrentExecutorNeedsNoCredentialWhenToolkitHasNoMatchingConfigurationField(t *testing.T) {
	candidate := scheduledExecutorCandidate(17)
	executor, dependencies := validCurrentExecutor(t, candidate)
	delete(
		dependencies.toolkits.toolkit.Settings,
		"github_configuration",
	)

	outcome, err := executor.ExecuteScheduled(
		context.Background(),
		candidate,
		time.Now(),
		"index-schedule-v1:no-provider-configuration",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionAdmitted ||
		dependencies.system.calls != 1 ||
		dependencies.start.calls != 1 {
		t.Fatalf(
			"outcome=%+v system calls=%d start calls=%d",
			outcome,
			dependencies.system.calls,
			dependencies.start.calls,
		)
	}
}

/* elitea_issues: #6527 — a schedule's explicitly chosen credential is never silently discarded when the toolkit type's settings key does not match the naive "{type}_configuration" derivation (e.g. ado_wiki/ado_repos store theirs under a shared group key) */
func TestCurrentExecutorFailsRatherThanDiscardingCredentialWhenConfigurationKeyMismatches(t *testing.T) {
	candidate := scheduledExecutorCandidate(17)
	private := false
	candidate.Schedule.Credentials = &Credentials{
		Private:     &private,
		EliteaTitle: "team-github",
	}
	executor, dependencies := validCurrentExecutor(t, candidate)
	// Simulate a toolkit type whose settings are not stored under
	// "{type}_configuration" (the ado_wiki/ado_repos/artifact/application
	// case from #6527) by removing the key the naive derivation looks for.
	delete(dependencies.toolkits.toolkit.Settings, "github_configuration")

	outcome, err := executor.ExecuteScheduled(
		context.Background(),
		candidate,
		time.Now(),
		"index-schedule-v1:configuration-key-mismatch",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != ExecutionInitializationFailed ||
		outcome.SafeReason != scheduleCredentialFailureReason {
		t.Fatalf("outcome=%+v — schedule's chosen credential must not be silently discarded", outcome)
	}
	if dependencies.start.calls != 0 {
		t.Fatalf("start.calls=%d — must not run with the wrong (toolkit's own) credential", dependencies.start.calls)
	}
}

func TestCurrentExecutorCredentialAndSystemPATFailuresAreTypedForHistory(t *testing.T) {
	personal := scheduledExecutorCandidate(17)
	tests := []struct {
		name       string
		mutate     func(*currentExecutorDependencies)
		wantReason string
	}{
		{
			name: "missing personal credential",
			mutate: func(dependencies *currentExecutorDependencies) {
				// Personal schedule plus an existing provider configuration
				// requires its explicit saved credential.
			},
			wantReason: scheduleCredentialFailureReason,
		},
		{
			name: "project system PAT unavailable",
			mutate: func(dependencies *currentExecutorDependencies) {
				private := false
				personal.Schedule.Credentials = &Credentials{
					Private:     &private,
					EliteaTitle: "project-github",
				}
				dependencies.system.err = errors.New("PAT secret canary")
			},
			wantReason: scheduleSystemPATFailureReason,
		},
		{
			name: "full unsecret preflight failed",
			mutate: func(dependencies *currentExecutorDependencies) {
				private := false
				personal.Schedule.Credentials = &Credentials{
					Private:     &private,
					EliteaTitle: "project-github",
				}
				dependencies.settings.err = errors.New("credential secret canary")
			},
			wantReason: scheduleCredentialFailureReason,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := personal
			candidate.Schedule.Credentials = personal.Schedule.Credentials
			executor, dependencies := validCurrentExecutor(t, candidate)
			test.mutate(dependencies)
			candidate.Schedule.Credentials = personal.Schedule.Credentials

			outcome, err := executor.ExecuteScheduled(
				context.Background(),
				candidate,
				time.Now(),
				"index-schedule-v1:failure",
			)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != ExecutionInitializationFailed ||
				outcome.SafeReason != test.wantReason {
				t.Fatalf("outcome=%+v", outcome)
			}
			if dependencies.start.calls != 0 ||
				dependencies.inspector.calls != 0 {
				t.Fatalf(
					"failure continued: starts=%d inspections=%d",
					dependencies.start.calls,
					dependencies.inspector.calls,
				)
			}
		})
	}
}

func TestCurrentExecutorPreservesExactIndexAndAdmissionDisposition(t *testing.T) {
	candidate := scheduledExecutorCandidate(-1)
	tests := []struct {
		name        string
		index       ScheduledIndex
		found       bool
		inspectErr  error
		start       indexingapp.StartOutcome
		startErr    error
		want        ExecutionDisposition
		wantErr     bool
		wantStarted bool
	}{
		{name: "missing", want: ExecutionSkippedUnavailable},
		{name: "empty state", found: true, index: ScheduledIndex{
			Configuration: map[string]any{"index_name": "docs"},
		}, want: ExecutionSkippedUnavailable},
		{name: "active case insensitive", found: true, index: ScheduledIndex{
			State: "In_Progress", Configuration: map[string]any{"index_name": "docs"},
		}, want: ExecutionSkippedActive},
		{name: "whitespace state and JSON string configuration", found: true, index: ScheduledIndex{
			State: " ", Configuration: `{"index_name":"docs","source":"all"}`,
		}, start: indexingapp.StartOutcome{
			TaskID: "new", Created: true,
		}, want: ExecutionAdmitted, wantStarted: true},
		{name: "inspection dependency", inspectErr: errors.New("pgvector unavailable"), wantErr: true},
		{name: "admitted", found: true, index: validScheduledIndex(), start: indexingapp.StartOutcome{
			TaskID: "new", Created: true,
		}, want: ExecutionAdmitted, wantStarted: true},
		{name: "idempotent", found: true, index: validScheduledIndex(), start: indexingapp.StartOutcome{
			TaskID: "existing",
		}, want: ExecutionIdempotent, wantStarted: true},
		{name: "active admission race", found: true, index: validScheduledIndex(),
			startErr: indexingapp.NewActiveIndexConflictError("active-task"),
			want:     ExecutionSkippedActive, wantStarted: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, dependencies := validCurrentExecutor(t, candidate)
			dependencies.inspector.index = test.index
			dependencies.inspector.found = test.found
			dependencies.inspector.err = test.inspectErr
			dependencies.start.outcome = test.start
			dependencies.start.err = test.startErr

			outcome, err := executor.ExecuteScheduled(
				context.Background(),
				candidate,
				time.Now(),
				"index-schedule-v1:disposition",
			)
			if test.wantErr != (err != nil) {
				t.Fatalf("outcome=%+v error=%v", outcome, err)
			}
			if !test.wantErr && outcome.Disposition != test.want {
				t.Fatalf("outcome=%+v want=%v", outcome, test.want)
			}
			if test.wantStarted != (dependencies.start.calls == 1) {
				t.Fatalf("start calls=%d", dependencies.start.calls)
			}
		})
	}
}

type currentExecutorDependencies struct {
	toolkits  *scheduledToolkitReaderStub
	settings  *scheduledSettingsStub
	system    *scheduledSystemIdentityStub
	inspector *scheduledIndexInspectorStub
	inputs    *scheduledInputResolverStub
	start     *scheduledStarterStub
}

func validCurrentExecutor(
	t *testing.T,
	candidate Candidate,
) (*CurrentExecutor, *currentExecutorDependencies) {
	t.Helper()
	dependencies := &currentExecutorDependencies{
		toolkits: &scheduledToolkitReaderStub{
			found: true,
			toolkit: indexingapp.CurrentToolkitSnapshot{
				ID: int32(candidate.ToolkitID), Type: candidate.ToolkitType,
				Name: "repository",
				Settings: map[string]any{
					"github_configuration": map[string]any{
						"elitea_title": "project-github",
						"private":      false,
					},
					"pgvector_configuration": map[string]any{
						"elitea_title": "project-vectorstore",
						"private":      false,
					},
				},
			},
		},
		settings: &scheduledSettingsStub{
			result: map[string]any{"resolved": true},
		},
		system: &scheduledSystemIdentityStub{},
		inspector: &scheduledIndexInspectorStub{
			found: true,
			index: validScheduledIndex(),
		},
		inputs: &scheduledInputResolverStub{
			inputs: validScheduledInputs(),
		},
		start: &scheduledStarterStub{
			outcome: indexingapp.StartOutcome{
				TaskID: "execution-1", Created: true,
			},
		},
	}
	executor := newCurrentExecutorForTest(
		t,
		dependencies.toolkits,
		dependencies.settings,
		dependencies.system,
		dependencies.inspector,
		dependencies.inputs,
		dependencies.start,
	)
	return executor, dependencies
}

func newCurrentExecutorForTest(
	t *testing.T,
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
	system ProjectSystemIdentityPreflight,
	inspector ScheduledIndexInspector,
	inputs ScheduledInputResolver,
	start ScheduledIndexStarter,
) *CurrentExecutor {
	t.Helper()
	executor, err := NewCurrentExecutor(
		toolkits,
		settings,
		system,
		inspector,
		inputs,
		start,
	)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func scheduledExecutorCandidate(userID int64) Candidate {
	createdBy := int64(99)
	if userID > 0 {
		createdBy = userID
	}
	return Candidate{
		ProjectID: 42, ToolkitID: 7, ToolkitType: "github",
		IndexMetaID: "docs", ScheduleUserID: userID,
		Schedule: Schedule{
			Cron: "0 3 * * *", Enabled: true, CreatedBy: createdBy,
			Timezone: "UTC", LastRun: "2026-07-27T03:00:00+00:00",
		},
	}
}

func validScheduledIndex() ScheduledIndex {
	return ScheduledIndex{
		State: "completed",
		Configuration: map[string]any{
			"index_name": "docs",
		},
	}
}

func validScheduledInputs() indexingapp.AuthoritativeInputs {
	return indexingapp.AuthoritativeInputs{
		ToolkitConfiguration: json.RawMessage(
			`{"id":7,"type":"github","settings":{}}`,
		),
		ToolParameters:   json.RawMessage(`{"index_name":"docs"}`),
		LLMConfiguration: json.RawMessage(`{}`),
	}
}

func containsBytes(value, substring []byte) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if reflect.DeepEqual(value[index:index+len(substring)], substring) {
			return true
		}
	}
	return false
}

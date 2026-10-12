package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type registryWriterStub struct {
	runs      []RegistryInitialRun
	terminals []RegistryTerminal
	stops     []RegistryManualStop
	err       error
	noRow     bool
}

func (s *registryWriterStub) InitializeRegistryRun(_ context.Context, run RegistryInitialRun) error {
	s.runs = append(s.runs, run)
	return s.err
}

func (s *registryWriterStub) ApplyRegistryTerminal(_ context.Context, t RegistryTerminal) error {
	s.terminals = append(s.terminals, t)
	return s.err
}

func (s *registryWriterStub) VerifyRegistryManualStop(_ context.Context, stop RegistryManualStop) (string, error) {
	s.stops = append(s.stops, stop)
	if s.noRow {
		return "", s.err
	}
	return "11111111-1111-1111-1111-111111111111", s.err
}

func registrySubmit() (SubmitRequest, AdmissionOutcome) {
	request := SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "7", ResourceProjectID: "7", ProjectionProjectID: "7", ActorID: "11",
		},
		CorrelationID: "corr-1",
		ToolkitID:     19,
		Initiator:     executiondomain.IndexIngestInitiatorUser,
		Inputs: AuthoritativeInputs{
			ToolkitConfiguration: json.RawMessage(`{"id":19,"type":"github","settings":{}}`),
			ToolParameters:       json.RawMessage(`{"index_name":"docs","progress_step":10}`),
		},
	}
	outcome := AdmissionOutcome{
		AdmissionOutcome:       executionapp.AdmissionOutcome{ExecutionID: "exec-1", AdmittedAt: time.Now()},
		Generation:             1,
		IndexGeneration:        3,
		IndexMetaID:            "meta-1",
		IndexMetaCorrelationID: "corr-1",
	}
	return request, outcome
}

// The registry initializer needs no pgvector_configuration and redeems no
// frozen toolkit: that is the point of the rust mode.
func TestRegistryInitializerWritesTheAdmittedRunWithoutAPgvectorTarget(t *testing.T) {
	writer := &registryWriterStub{}
	initializer, err := NewRegistryIndexMetaInitializer(writer)
	if err != nil {
		t.Fatal(err)
	}
	request, outcome := registrySubmit()
	if err := initializer.MaterializeInitialIndexMeta(context.Background(), request, outcome); err != nil {
		t.Fatal(err)
	}
	if len(writer.runs) != 1 {
		t.Fatalf("runs = %+v", writer.runs)
	}
	run := writer.runs[0]
	if run.ProjectID != 7 || run.ToolkitID != 19 || run.IndexName != "docs" || run.MetaID != "meta-1" ||
		run.ExecutionID != "exec-1" || run.Generation != 1 || run.IndexGeneration != 3 || run.CorrelationID != "corr-1" ||
		string(run.Configuration) != `{"index_name":"docs","progress_step":10}` {
		t.Fatalf("run = %+v", run)
	}
}

func TestRegistryInitializerRefusesWhatTheStartPathNeverAdmits(t *testing.T) {
	cases := map[string]func(*SubmitRequest, *AdmissionOutcome){
		"tenant differs from the project": func(r *SubmitRequest, _ *AdmissionOutcome) { r.Identity.TenantID = "8" },
		"correlation differs":             func(_ *SubmitRequest, o *AdmissionOutcome) { o.IndexMetaCorrelationID = "other" },
		"no index generation":             func(_ *SubmitRequest, o *AdmissionOutcome) { o.IndexGeneration = 0 },
		"no execution":                    func(_ *SubmitRequest, o *AdmissionOutcome) { o.ExecutionID = "" },
		"no index name": func(r *SubmitRequest, _ *AdmissionOutcome) {
			r.Inputs.ToolParameters = json.RawMessage(`{"progress_step":10}`)
		},
		"non-numeric project": func(r *SubmitRequest, _ *AdmissionOutcome) {
			r.Identity.ResourceProjectID, r.Identity.TenantID, r.Identity.ProjectionProjectID = "x", "x", "x"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			writer := &registryWriterStub{}
			initializer, _ := NewRegistryIndexMetaInitializer(writer)
			request, outcome := registrySubmit()
			mutate(&request, &outcome)
			err := initializer.MaterializeInitialIndexMeta(context.Background(), request, outcome)
			if !errors.Is(err, ErrCurrentIndexMetaInitializationInvalid) || len(writer.runs) != 0 {
				t.Fatalf("err = %v, writes = %d", err, len(writer.runs))
			}
		})
	}
}

func TestRegistryInitializerMapsWriterFailuresToTheSameErrorsAsThePythonPath(t *testing.T) {
	for _, tc := range []struct{ in, want error }{
		{ErrCurrentIndexMetaConflict, ErrCurrentIndexMetaConflict},
		{ErrCurrentIndexMetaSuperseded, ErrCurrentIndexMetaSuperseded},
		{errors.New("connection reset"), ErrCurrentIndexMetaMaterializationUnavailable},
	} {
		writer := &registryWriterStub{err: tc.in}
		initializer, _ := NewRegistryIndexMetaInitializer(writer)
		request, outcome := registrySubmit()
		if err := initializer.MaterializeInitialIndexMeta(context.Background(), request, outcome); !errors.Is(err, tc.want) {
			t.Fatalf("writer error %v became %v, want %v", tc.in, err, tc.want)
		}
	}
}

type bindingsStub struct {
	binding CurrentIndexMetaTerminalBinding
}

func (s bindingsStub) LoadCurrentIndexMetaTerminalBinding(context.Context, string, uint64) (CurrentIndexMetaTerminalBinding, error) {
	return s.binding, nil
}

func registryBinding() CurrentIndexMetaTerminalBinding {
	return CurrentIndexMetaTerminalBinding{
		ResourceProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexName: "docs",
		MetaID: "meta-1", ExecutionID: "exec-1", Generation: 1, IndexGeneration: 3,
		ToolkitConfiguration: json.RawMessage(`{"id":19}`),
	}
}

func TestRegistryTerminalizerAppliesTheBindingWithoutRedeemingTheToolkit(t *testing.T) {
	writer := &registryWriterStub{}
	terminalizer, err := NewRegistryIndexMetaTerminalizer(bindingsStub{registryBinding()}, writer)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := terminalizer.Terminalize(context.Background(), CurrentIndexMetaTerminalRequest{
		ExecutionID: "exec-1", Generation: 1, State: CurrentIndexMetaFailed, OccurredAt: at, SafeError: "worker lost",
	}); err != nil {
		t.Fatal(err)
	}
	if len(writer.terminals) != 1 {
		t.Fatalf("terminals = %+v", writer.terminals)
	}
	got := writer.terminals[0]
	if got.ProjectID != 7 || got.IndexName != "docs" || got.ToolkitID != 19 || got.MetaID != "meta-1" ||
		got.IndexGeneration != 3 || got.State != CurrentIndexMetaFailed || got.SafeError != "worker lost" {
		t.Fatalf("terminal = %+v", got)
	}
	// A binding for another execution is not the request's.
	other := registryBinding()
	other.ExecutionID = "exec-2"
	mismatched, _ := NewRegistryIndexMetaTerminalizer(bindingsStub{other}, writer)
	if err := mismatched.Terminalize(context.Background(), CurrentIndexMetaTerminalRequest{
		ExecutionID: "exec-1", Generation: 1, State: CurrentIndexMetaCancelled, OccurredAt: at,
	}); !errors.Is(err, ErrCurrentIndexMetaConflict) {
		t.Fatalf("mismatched binding = %v, want conflict", err)
	}
}

// A manual stop of a reindex only verifies that the registry row is cancelled
// for the stopped run. The cleaner has no vector deleter to call at all: the
// index's vectors, and its index_registry_documents rows, stay as the run left
// them, so a stopped reindex of a completed index is still searchable and the
// next run reconciles incrementally.
func TestRegistryManualStopCleanerKeepsTheIndexVectorsAndOnlyVerifiesTheStop(t *testing.T) {
	writer := &registryWriterStub{}
	cleaner, err := NewRegistryManualStopCleaner(bindingsStub{registryBinding()}, writer)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleaner.Cleanup(context.Background(), CurrentManualStopCleanupRequest{ExecutionID: "exec-1", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if len(writer.stops) != 1 || writer.stops[0].IndexName != "docs" || writer.stops[0].ProjectID != 7 {
		t.Fatalf("stops = %+v", writer.stops)
	}

	// A row that does not verify (not cancelled for this run) is refused, and
	// retried by the effect.
	writer.err = ErrCurrentIndexMetaConflict
	if err := cleaner.Cleanup(context.Background(), CurrentManualStopCleanupRequest{ExecutionID: "exec-1", Generation: 1}); !errors.Is(err, ErrCurrentIndexMetaConflict) {
		t.Fatalf("unverified stop: err=%v", err)
	}

	// No row names the run (admitted under python before the mode switched): the
	// writer acknowledges it with no index id, and the effect resolves.
	writer.err = nil
	writer.noRow = true
	if err := cleaner.Cleanup(context.Background(), CurrentManualStopCleanupRequest{ExecutionID: "exec-1", Generation: 1}); err != nil {
		t.Fatalf("a stop with no registry row must resolve, got %v", err)
	}
}

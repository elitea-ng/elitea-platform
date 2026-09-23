package output

import (
	"context"
	"errors"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type discoveryBindingStub struct{ expected ExpectedToolkitAvailableTools }

func (s discoveryBindingStub) ExpectedToolkitAvailableTools(context.Context, string, uint64) (ExpectedToolkitAvailableTools, error) {
	return s.expected, nil
}

type discoveryProjectorStub struct {
	calls   int
	outcome ProjectionOutcome
	err     error
	frame   ToolkitAvailableToolsFrame
}

func (s *discoveryProjectorStub) ProjectToolkitAvailableTools(_ context.Context, p ToolkitAvailableToolsProjection) (ProjectionOutcome, error) {
	s.calls++
	s.frame = p.Frame
	return s.outcome, s.err
}

func discoveryOutputFixture() (ToolkitAvailableToolsFrame, ExpectedToolkitAvailableTools) {
	base, _ := validValidationOutput()
	logical := "toolkit-available-tools:" + base.Fence.ExecutionID
	base.Settlement.TerminalLogicalOutputID = logical
	settings := ToolkitAvailableToolsInputBinding{EntryID: "settings", ImmutableVersion: "version-1", ContentDigest: runtimedomain.SHA256([]byte("settings"))}
	frame := ToolkitAvailableToolsFrame{
		StreamID: base.StreamID, TenantID: base.TenantID, ResourceProjectID: base.ResourceProjectID, ProjectionProjectID: base.ProjectionProjectID,
		WorkloadSessionID: base.WorkloadSessionID, ProducerID: base.ProducerID, EventID: base.EventID, LogicalOutputID: logical, Sequence: base.Sequence, OccurredAt: base.OccurredAt,
		Fence: base.Fence, PayloadDigest: base.PayloadDigest, EncodedResult: base.EncodedResult, Settlement: base.Settlement, EncodedSettlement: base.EncodedSettlement,
		Result: ToolkitAvailableToolsResult{ToolkitType: "github", InputBundleID: "bundle-1", InputBundleDigest: runtimedomain.SHA256([]byte("bundle")), Settings: settings,
			ResultArtifact: ToolkitAvailableToolsArtifact{ArtifactID: "artifact-1", ImmutableVersion: "version-1", MediaType: ToolkitAvailableToolsMediaType, Classification: ToolkitAvailableToolsClassification, ByteLength: 2, Digest: runtimedomain.SHA256([]byte("[]"))}},
	}
	expected := ExpectedToolkitAvailableTools{TenantID: frame.TenantID, ResourceProjectID: frame.ResourceProjectID, ProjectionProjectID: frame.ProjectionProjectID, CapabilityID: executiondomain.ToolkitAvailableToolsCapability,
		CommandID: frame.Fence.CommandID, ExecutionID: frame.Fence.ExecutionID, Generation: frame.Fence.Generation, LogicalOutputID: logical, InputBundleID: frame.Result.InputBundleID, InputBundleDigest: frame.Result.InputBundleDigest, Settings: settings, ToolkitType: "github"}
	return frame, expected
}

func TestToolkitAvailableToolsChecksAdmittedBindings(t *testing.T) {
	mutations := map[string]func(*ExpectedToolkitAvailableTools){
		"tenant":             func(e *ExpectedToolkitAvailableTools) { e.TenantID = "other" },
		"resource project":   func(e *ExpectedToolkitAvailableTools) { e.ResourceProjectID = "other" },
		"projection project": func(e *ExpectedToolkitAvailableTools) { e.ProjectionProjectID = "other" },
		"command":            func(e *ExpectedToolkitAvailableTools) { e.CommandID = "other" },
		"generation":         func(e *ExpectedToolkitAvailableTools) { e.Generation++ },
		"toolkit type":       func(e *ExpectedToolkitAvailableTools) { e.ToolkitType = "jira" },
		"bundle":             func(e *ExpectedToolkitAvailableTools) { e.InputBundleID = "other" },
		"bundle digest":      func(e *ExpectedToolkitAvailableTools) { e.InputBundleDigest = runtimedomain.SHA256([]byte("other")) },
		"settings entry":     func(e *ExpectedToolkitAvailableTools) { e.Settings.EntryID = "other" },
		"settings version":   func(e *ExpectedToolkitAvailableTools) { e.Settings.ImmutableVersion = "other" },
		"settings digest": func(e *ExpectedToolkitAvailableTools) {
			e.Settings.ContentDigest = runtimedomain.SHA256([]byte("other"))
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			frame, expected := discoveryOutputFixture()
			mutate(&expected)
			projector := &discoveryProjectorStub{}
			service, err := NewToolkitAvailableToolsService(discoveryBindingStub{expected}, fenceVerifierStub{}, projector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = service.IngestToolkitAvailableTools(context.Background(), frame); !errors.Is(err, ErrToolkitAvailableToolsBindingMismatch) {
				t.Fatalf("error=%v", err)
			}
			if projector.calls != 0 {
				t.Fatal("mismatched output reached projector")
			}
		})
	}
}

func TestToolkitAvailableToolsPreservesFenceReplayAndConflictOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fenceErr, error error
		inserted        bool
	}{
		{name: "insert", inserted: true}, {name: "exact replay"}, {name: "stale fence", fenceErr: runtimedomain.ErrStaleFence}, {name: "conflicting replay", error: ErrToolkitAvailableToolsOutputConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, expected := discoveryOutputFixture()
			projector := &discoveryProjectorStub{outcome: ProjectionOutcome{CommittedSequence: frame.Sequence, Inserted: tc.inserted}, err: tc.error}
			service, err := NewToolkitAvailableToolsService(discoveryBindingStub{expected}, fenceVerifierStub{err: tc.fenceErr}, projector)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.IngestToolkitAvailableTools(context.Background(), frame)
			wantErr := tc.error
			if tc.fenceErr != nil {
				wantErr = tc.fenceErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if tc.fenceErr != nil && projector.calls != 0 {
				t.Fatal("stale fence reached projector")
			}
			if err == nil && (result.CommittedSequence != frame.Sequence || result.Inserted != tc.inserted) {
				t.Fatalf("outcome=%+v", result)
			}
			if err == nil {
				frame.EncodedResult[0] ^= 1
				frame.EncodedSettlement[0] ^= 1
				if projector.frame.EncodedResult[0] == frame.EncodedResult[0] || projector.frame.EncodedSettlement[0] == frame.EncodedSettlement[0] {
					t.Fatal("projected bytes alias caller memory")
				}
			}
		})
	}
}

func TestToolkitAvailableToolsRejectsMalformedArtifactsAndFailedSettlement(t *testing.T) {
	mutations := map[string]func(*ToolkitAvailableToolsFrame){
		"empty artifact": func(f *ToolkitAvailableToolsFrame) { f.Result.ResultArtifact = ToolkitAvailableToolsArtifact{} },
		"oversized artifact": func(f *ToolkitAvailableToolsFrame) {
			f.Result.ResultArtifact.ByteLength = MaxToolkitAvailableToolsArtifactBytes + 1
		},
		"wrong media":              func(f *ToolkitAvailableToolsFrame) { f.Result.ResultArtifact.MediaType = "application/json" },
		"wrong classification":     func(f *ToolkitAvailableToolsFrame) { f.Result.ResultArtifact.Classification = "public" },
		"failed settlement":        func(f *ToolkitAvailableToolsFrame) { f.Settlement.Outcome = executionapp.SettlementFailed },
		"noncanonical event":       func(f *ToolkitAvailableToolsFrame) { f.EventID = "other" },
		"modified payload":         func(f *ToolkitAvailableToolsFrame) { f.EncodedResult = []byte("other") },
		"invalid settings version": func(f *ToolkitAvailableToolsFrame) { f.Result.Settings.ImmutableVersion = "\n" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			frame, _ := discoveryOutputFixture()
			mutate(&frame)
			if !errors.Is(frame.Validate(), ErrInvalidToolkitAvailableToolsOutput) {
				t.Fatalf("validation error=%v", frame.Validate())
			}
		})
	}
}

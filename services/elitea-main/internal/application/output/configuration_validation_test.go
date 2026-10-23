package output

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type bindingRepositoryStub struct {
	expected ExpectedValidation
	lookups  []uint64
}

func (s *bindingRepositoryStub) ExpectedValidation(_ context.Context, executionID string, generation uint64) (ExpectedValidation, error) {
	s.lookups = append(s.lookups, generation)
	if executionID != s.expected.ExecutionID || generation != s.expected.Generation {
		return ExpectedValidation{}, runtimedomain.ErrStaleFence
	}
	return s.expected, nil
}

type fenceVerifierStub struct {
	expected *runtimedomain.Fence
	err      error
}

func (s fenceVerifierStub) VerifyActive(_ context.Context, fence runtimedomain.Fence) error {
	if s.expected != nil && *s.expected != fence {
		return runtimedomain.ErrStaleFence
	}
	return s.err
}

type memoryValidationProjector struct {
	byEvent   map[string]ValidationProjection
	byLogical map[string]ValidationProjection
	cursor    uint64
	calls     int
}

func newMemoryValidationProjector() *memoryValidationProjector {
	return &memoryValidationProjector{
		byEvent:   make(map[string]ValidationProjection),
		byLogical: make(map[string]ValidationProjection),
	}
}

func (p *memoryValidationProjector) ProjectConfigurationValidation(_ context.Context, projection ValidationProjection) (ProjectionOutcome, error) {
	p.calls++
	frame := projection.Frame
	if existing, ok := p.byEvent[frame.EventID]; ok {
		if !sameOutput(existing.Frame, frame) {
			return ProjectionOutcome{}, ErrValidationOutputConflict
		}
		return ProjectionOutcome{Inserted: false, Cursor: p.cursor, CommittedSequence: existing.Frame.Sequence}, nil
	}
	if existing, ok := p.byLogical[frame.LogicalOutputID]; ok {
		if !sameOutput(existing.Frame, frame) {
			return ProjectionOutcome{}, ErrValidationOutputConflict
		}
		return ProjectionOutcome{Inserted: false, Cursor: p.cursor, CommittedSequence: existing.Frame.Sequence}, nil
	}
	p.cursor++
	p.byEvent[frame.EventID] = projection
	p.byLogical[frame.LogicalOutputID] = projection
	return ProjectionOutcome{Inserted: true, Cursor: p.cursor, CommittedSequence: frame.Sequence}, nil
}

func sameOutput(a, b ConfigurationValidationFrame) bool {
	return a.Fence == b.Fence && a.PayloadDigest == b.PayloadDigest && a.EventID == b.EventID && a.LogicalOutputID == b.LogicalOutputID
}

func TestConfigurationValidationIngestChecksAllBindingsAndIsIdempotent(t *testing.T) {
	frame, expected := validValidationOutput()
	projector := newMemoryValidationProjector()
	bindings := &bindingRepositoryStub{expected: expected}
	service, err := NewConfigurationValidationService(bindings, fenceVerifierStub{}, projector)
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.Ingest(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted || first.Cursor != 1 || first.CommittedSequence != frame.Sequence {
		t.Fatalf("unexpected first projection: %+v", first)
	}
	replayed, err := service.Ingest(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Inserted || replayed.Cursor != first.Cursor {
		t.Fatalf("identical replay was not idempotent: %+v", replayed)
	}

	stored := projector.byEvent[frame.EventID]
	if string(stored.BrowserData) == "" || containsSensitiveProjection(string(stored.BrowserData)) {
		t.Fatalf("unsafe browser projection: %s", stored.BrowserData)
	}

	mismatch := frame
	mismatch.Result.Binding.InputBundleDigest = runtimedomain.SHA256([]byte("other-bundle"))
	if _, err := service.Ingest(context.Background(), mismatch); !errors.Is(err, configurationdomain.ErrValidationBindingMismatch) {
		t.Fatalf("expected binding mismatch, got %v", err)
	}
	if projector.calls != 2 {
		t.Fatalf("binding mismatch reached projector; calls=%d", projector.calls)
	}
	if len(bindings.lookups) != 3 || bindings.lookups[0] != frame.Fence.Generation {
		t.Fatalf("binding repository was not keyed by generation: %v", bindings.lookups)
	}
}

func TestConfigurationValidationIngestRejectsStaleFenceBeforeProjection(t *testing.T) {
	frame, expected := validValidationOutput()
	projector := newMemoryValidationProjector()
	service, err := NewConfigurationValidationService(
		&bindingRepositoryStub{expected: expected},
		fenceVerifierStub{err: runtimedomain.ErrStaleFence},
		projector,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ingest(context.Background(), frame); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected stale fence, got %v", err)
	}
	if projector.calls != 0 {
		t.Fatal("stale output reached projector")
	}
}

func TestConfigurationValidationProjectorConflictIsPreserved(t *testing.T) {
	frame, expected := validValidationOutput()
	projector := newMemoryValidationProjector()
	service, err := NewConfigurationValidationService(&bindingRepositoryStub{expected: expected}, fenceVerifierStub{}, projector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ingest(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	conflict := frame
	conflict.EncodedResult = []byte("different-encoded-result")
	conflict.PayloadDigest = runtimedomain.SHA256(conflict.EncodedResult)
	conflict.Settlement.TerminalPayloadDigest = conflict.PayloadDigest
	conflict.EncodedSettlement = []byte("different-settlement-protobuf")
	conflict.Settlement.ProposalDigest = runtimedomain.SHA256(conflict.EncodedSettlement)
	_, err = service.Ingest(context.Background(), conflict)
	if !errors.Is(err, ErrValidationOutputConflict) {
		t.Fatalf("expected logical output conflict, got %v", err)
	}
}

func TestConfigurationValidationIngestRejectsWrongProducerTokenAndGeneration(t *testing.T) {
	base, expected := validValidationOutput()
	tests := []struct {
		name   string
		mutate func(*ConfigurationValidationFrame)
	}{
		{name: "producer", mutate: func(frame *ConfigurationValidationFrame) {
			frame.ProducerID = "worker-2"
			frame.Fence.ProducerID = "worker-2"
		}},
		{name: "token", mutate: func(frame *ConfigurationValidationFrame) {
			frame.Fence.Token = runtimedomain.FenceToken(runtimedomain.SHA256([]byte("wrong-token")))
		}},
		{name: "generation", mutate: func(frame *ConfigurationValidationFrame) {
			frame.Fence.Generation++
			frame.StreamID = frame.Fence.ExecutionID + ":2"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := base
			test.mutate(&frame)
			frame.Settlement.Fence = frame.Fence
			projector := newMemoryValidationProjector()
			service, err := NewConfigurationValidationService(
				&bindingRepositoryStub{expected: expected},
				fenceVerifierStub{expected: &base.Fence},
				projector,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Ingest(context.Background(), frame); !errors.Is(err, runtimedomain.ErrStaleFence) {
				t.Fatalf("expected stale fence, got %v", err)
			}
			if projector.calls != 0 {
				t.Fatal("stale output reached projector")
			}
		})
	}
}

func TestConfigurationValidationFrameRejectsNonCanonicalTerminalIdentities(t *testing.T) {
	base, _ := validValidationOutput()
	tests := []struct {
		name   string
		mutate func(*ConfigurationValidationFrame)
	}{
		{name: "stream", mutate: func(frame *ConfigurationValidationFrame) {
			frame.StreamID = "other-stream"
		}},
		{name: "event", mutate: func(frame *ConfigurationValidationFrame) {
			frame.EventID = "other-event"
			frame.Settlement.TerminalEventID = frame.EventID
		}},
		{name: "logical output", mutate: func(frame *ConfigurationValidationFrame) {
			frame.LogicalOutputID = "other-logical-output"
			frame.Settlement.TerminalLogicalOutputID = frame.LogicalOutputID
		}},
		{name: "sequence", mutate: func(frame *ConfigurationValidationFrame) {
			frame.Sequence = 2
			frame.Settlement.TerminalSequence = frame.Sequence
		}},
		{name: "proposal", mutate: func(frame *ConfigurationValidationFrame) {
			frame.Settlement.ProposalID = "other-proposal"
		}},
		{name: "settlement key", mutate: func(frame *ConfigurationValidationFrame) {
			frame.Settlement.IdempotencyKey = "other-key"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := base
			test.mutate(&frame)
			frame.EncodedSettlement = []byte("internally-consistent-" + test.name)
			frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
			if err := frame.Validate(); !errors.Is(err, ErrInvalidValidationOutput) {
				t.Fatalf("expected non-canonical terminal identity rejection, got %v", err)
			}
		})
	}
}

func TestConfigurationValidationFrameAllowsCanonicalTerminalAfterProgress(t *testing.T) {
	frame, _ := validValidationOutput()
	frame.Sequence = 2
	frame.EventID = frame.Fence.CommandID + ":2"
	frame.Settlement.TerminalSequence = 2
	frame.Settlement.TerminalEventID = frame.EventID
	if err := frame.Validate(); err != nil {
		t.Fatalf("canonical later terminal sequence was rejected: %v", err)
	}
}

func validValidationOutput() (ConfigurationValidationFrame, ExpectedValidation) {
	binding := configurationdomain.ValidationBinding{
		Command: configurationdomain.ValidationCommand{
			ConfigurationRevisionID: "revision-1",
			ConfigurationType:       "openapi",
			CatalogRevision:         "sdk-commit",
			CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
			SchemaID:                "openapi",
			SchemaRevision:          "schema-v1",
			SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
			SettingsEntryID:         "settings",
		},
		InputBundleID:         "bundle-1",
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		SettingsEntryVersion:  "revision-1",
		SettingsContentDigest: runtimedomain.SHA256([]byte(`{"auth_type":"Digest"}`)),
	}
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unpredictable-test-token"))),
	}
	encoded := []byte("deterministic-result-protobuf")
	encodedSettlement := []byte("deterministic-settlement-protobuf")
	payloadDigest := runtimedomain.SHA256(encoded)
	frame := ConfigurationValidationFrame{
		StreamID:            "execution-1:1",
		TenantID:            "tenant-1",
		ResourceProjectID:   "project-1",
		ProjectionProjectID: "project-1",
		WorkloadSessionID:   "workload-1",
		ProducerID:          "worker-1",
		EventID:             "command-1:1",
		LogicalOutputID:     "configuration-validation:revision-1",
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
		Fence:               fence,
		PayloadDigest:       payloadDigest,
		EncodedResult:       encoded,
		Settlement: executionapp.SettlementProposal{
			Fence:                   fence,
			ProposalID:              "command-1:settlement",
			Outcome:                 executionapp.SettlementSucceeded,
			TerminalLogicalOutputID: "configuration-validation:revision-1",
			TerminalEventID:         "command-1:1",
			TerminalSequence:        1,
			TerminalPayloadDigest:   payloadDigest,
			ProposalDigest:          runtimedomain.SHA256(encodedSettlement),
			IdempotencyKey:          "command-1:prepare-settlement",
		},
		EncodedSettlement: encodedSettlement,
		Result: configurationdomain.ValidationResult{
			Binding: binding,
			Valid:   false,
			Issues: []configurationdomain.ValidationIssue{{
				Code:        "VALUE_NOT_ALLOWED",
				JSONPointer: "/auth_type",
				SafeMessage: "Value is not one of the allowed choices.",
			}},
		},
	}
	expected := ExpectedValidation{
		TenantID:            frame.TenantID,
		ResourceProjectID:   frame.ResourceProjectID,
		ProjectionProjectID: frame.ProjectionProjectID,
		CommandID:           fence.CommandID,
		ExecutionID:         fence.ExecutionID,
		Generation:          fence.Generation,
		Binding:             binding,
	}
	return frame, expected
}

func containsSensitiveProjection(value string) bool {
	for _, forbidden := range []string{"Digest", "settings_content_digest", "catalog_digest", "schema_digest"} {
		if strings.Contains(value, forbidden) {
			return true
		}
	}
	return false
}

package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type indexAdmissionStoreStub struct {
	admission Admission
}

func (s *indexAdmissionStoreStub) AdmitIndexIngest(_ context.Context, admission Admission) (AdmissionOutcome, error) {
	s.admission = admission
	admittedAt := time.Date(2026, time.July, 22, 9, 0, 0, 0, time.UTC)
	return AdmissionOutcome{
		AdmissionOutcome: executionapp.AdmissionOutcome{
			ExecutionID: admission.Record.Job.ID,
			CommandID:   admission.Record.Job.CommandID,
			Created:     true,
			AdmittedAt:  admittedAt,
			Deadline:    admittedAt.Add(time.Hour),
		},
		Generation:             admission.Record.Job.Generation,
		IndexGeneration:        1,
		IndexMetaID:            admission.Binding.IndexMetaID,
		IndexMetaCorrelationID: admission.Binding.IndexMetaCorrelationID,
	}, nil
}

func TestInputBundleFactoryBuildsTypedReferenceManifestWithoutAliasing(t *testing.T) {
	ids := sequenceIDs("bundle", "toolkit-content", "parameters-content", "model-content", "llm-content", "mcp-content")
	factory, err := NewInputBundleFactory(InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, ids)
	if err != nil {
		t.Fatal(err)
	}
	model := "gpt-test"
	inputs := AuthoritativeInputs{
		ToolkitConfiguration: json.RawMessage(`{"id":19,"type":"confluence","settings":{"token":"secret-ref://42"}}`),
		ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		LLMModel:             &model,
		LLMConfiguration:     json.RawMessage(`{"temperature":0.1}`),
		MCPReferences:        json.RawMessage(`{"server":"credential-ref://7"}`),
	}
	bundle, binding, err := factory.Build(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Entries) != 5 || binding.ToolkitConfigurationEntryID == "" || binding.ToolParametersEntryID == "" || binding.LLMModelEntryID == "" || binding.LLMConfigurationEntryID == "" || binding.MCPTokensEntryID == "" {
		t.Fatalf("unexpected typed index bundle: entries=%d binding=%+v", len(bundle.Entries), binding)
	}
	var manifest runtimev1.ExecutionInputBundleV1
	if err := proto.Unmarshal(bundle.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.GetEntries()) != 5 {
		t.Fatalf("manifest entry count=%d, want 5", len(manifest.GetEntries()))
	}
	for index, entry := range manifest.GetEntries() {
		if entry.GetContent() == nil || len(entry.GetContent().GetDigest().GetValue()) != 32 || entry.GetContent().GetByteLength() != uint64(bundle.Entries[index].ContentLength) {
			t.Fatalf("manifest entry %d lost reference binding: %v", index, entry)
		}
	}
	inputs.ToolkitConfiguration[2] = 'X'
	if bytes.Equal(bundle.Entries[0].Content, inputs.ToolkitConfiguration) {
		t.Fatal("durable toolkit configuration aliases resolver memory")
	}
}

func TestInputBundleFactoryBindsEmbeddingByReferenceAndDigest(t *testing.T) {
	factory, err := NewInputBundleFactory(InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, sequenceIDs("bundle", "toolkit-content", "parameters-content", "embedding-content"))
	if err != nil {
		t.Fatal(err)
	}
	configurationDigest := runtimedomain.SHA256([]byte("non-secret-configuration"))
	binding := EmbeddingBinding{
		SchemaVersion:          CurrentEmbeddingBindingSchema,
		ModelName:              "text-embedding-3-small",
		ResolvedModelGroup:     "1_text-embedding-3-small",
		Route:                  "public",
		ConfigurationProjectID: 1,
		ConfigurationUUID:      "00000000-0000-0000-0000-000000000101",
		ConfigurationDigest:    configurationDigest,
	}
	bundle, indexBinding, err := factory.Build(context.Background(), AuthoritativeInputs{
		ToolkitConfiguration: json.RawMessage(`{"id":19,"type":"github"}`),
		ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		EmbeddingBinding:     &binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Entries) != 3 ||
		indexBinding.EmbeddingBindingEntryID != indexEmbeddingBindingEntryID ||
		indexBinding.EmbeddingBindingDigest.IsZero() ||
		indexBinding.EmbeddingBindingDigest != bundle.Entries[2].ContentDigest ||
		bundle.Entries[2].SemanticRole != executiondomain.IndexEmbeddingBindingRole {
		t.Fatalf("bundle=%+v binding=%+v", bundle, indexBinding)
	}
	indexBinding.IndexMetaID = "index-meta-1"
	indexBinding.IndexMetaCorrelationID = "index-correlation-1"
	indexBinding.ToolkitID = 19
	indexBinding.IndexName = "docs"
	indexBinding.Initiator = executiondomain.IndexIngestInitiatorUser
	if err := indexBinding.Validate(bundle); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle.Entries[2].Content), "credential-current") {
		t.Fatalf("embedding binding copied a configuration reference: %s", bundle.Entries[2].Content)
	}
}

func TestAdmissionServiceBuildsIndexJobAndPreservesCurrentIdentity(t *testing.T) {
	factory, err := NewInputBundleFactory(InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, sequenceIDs("bundle-1", "toolkit-content", "parameters-content"))
	if err != nil {
		t.Fatal(err)
	}
	store := &indexAdmissionStoreStub{}
	service, err := NewAdmissionService(store, factory, func() time.Time {
		return time.Date(2026, time.July, 22, 8, 0, 0, 0, time.UTC)
	}, sequenceIDs("execution-1", "command-1", "outbox-1", "index-meta-1"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Submit(context.Background(), SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-1",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "7",
		},
		IdempotencyKey:  "index-request-1",
		CorrelationID:   "message-1",
		ClientStreamID:  "stream-1",
		ClientMessageID: "message-1",
		SIOEvent:        CurrentIndexSIOEvent,
		ToolkitID:       19,
		Initiator:       executiondomain.IndexIngestInitiatorUser,
		Inputs: AuthoritativeInputs{
			ToolkitConfiguration: json.RawMessage(`{"id":19,"type":"confluence"}`),
			ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Created || outcome.ExecutionID != "execution-1" || outcome.CommandID != "command-1" {
		t.Fatalf("unexpected admission outcome: %+v", outcome)
	}
	if store.admission.Record.Job.CapabilityID != executiondomain.IndexIngestCapability ||
		store.admission.Binding.ToolkitID != 19 ||
		store.admission.Binding.IndexName != "docs" ||
		store.admission.Binding.IndexMetaID != "index-meta-1" ||
		store.admission.Binding.IndexMetaCorrelationID != "message-1" ||
		store.admission.Binding.ClientStreamID != "stream-1" ||
		store.admission.Binding.ClientMessageID != "message-1" ||
		store.admission.Binding.SIOEvent != CurrentIndexSIOEvent ||
		store.admission.Binding.Initiator != executiondomain.IndexIngestInitiatorUser {
		t.Fatalf("index identity was not preserved: %+v", store.admission)
	}
	if err := store.admission.Binding.Validate(store.admission.Record.InputBundle); err != nil {
		t.Fatalf("stored binding is invalid: %v", err)
	}
}

func TestIndexAdmissionRejectsContentBeyondDurableEntryLimit(t *testing.T) {
	value := bytes.Repeat([]byte{'x'}, executiondomain.MaxInputEntryContentBytes+1)
	inputs := AuthoritativeInputs{
		ToolkitConfiguration: json.RawMessage(`{"id":1}`),
		ToolParameters:       append(json.RawMessage(`{"index_name":"`), append(value, []byte(`"}`)...)...),
	}
	if err := inputs.validate(); !errors.Is(err, ErrInvalidAuthoritativeIndexInput) {
		t.Fatalf("oversized entry error=%v, want %v", err, ErrInvalidAuthoritativeIndexInput)
	}
}

func TestIndexMetaInitializationRequiresExactBoundedIdentity(t *testing.T) {
	valid := IndexMetaInitialization{
		ExecutionID:     "execution-1",
		Generation:      1,
		IndexGeneration: 1,
		MetaID:          "index-meta-1",
		CorrelationID:   "message-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []IndexMetaInitialization{
		func() IndexMetaInitialization { value := valid; value.ExecutionID = ""; return value }(),
		func() IndexMetaInitialization { value := valid; value.Generation = 0; return value }(),
		func() IndexMetaInitialization { value := valid; value.IndexGeneration = 0; return value }(),
		func() IndexMetaInitialization { value := valid; value.MetaID = ""; return value }(),
		func() IndexMetaInitialization { value := valid; value.CorrelationID = ""; return value }(),
		func() IndexMetaInitialization { value := valid; value.CorrelationID = "message\n2"; return value }(),
		func() IndexMetaInitialization {
			value := valid
			value.MetaID = string(bytes.Repeat([]byte{'x'}, executiondomain.MaxIndexMetaIDBytes+1))
			return value
		}(),
		func() IndexMetaInitialization {
			value := valid
			value.CorrelationID = string(bytes.Repeat([]byte{'x'}, executiondomain.MaxIndexMetaCorrelationBytes+1))
			return value
		}(),
	}
	for index, initialization := range tests {
		if err := initialization.Validate(); !errors.Is(err, ErrIndexMetaInitializationMismatch) {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
}

func sequenceIDs(values ...string) executionapp.IDGenerator {
	return func() (string, error) {
		if len(values) == 0 {
			return "", fmt.Errorf("unexpected ID allocation")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
}

package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

// Offline owner fixture. Production reference mint/read remains Main's sole
// capture repository; the service is exercised through its actual entry points.
type originalSourceOwnerFixture struct {
	captures         []RootSourceCaptureRequest
	restores         []RootSourceRestoreRequest
	source           scope.SourceDefinition
	applicationTools json.RawMessage
	err              error
	events           *[]string
}

func (owner *originalSourceOwnerFixture) CaptureOriginalRootSource(_ context.Context, request RootSourceCaptureRequest) (scope.SourceReference, error) {
	request.VersionDetails = bytes.Clone(request.VersionDetails)
	owner.captures = append(owner.captures, request)
	if owner.events != nil {
		*owner.events = append(*owner.events, "capture")
	}
	if owner.err != nil {
		return scope.SourceReference{}, owner.err
	}
	wire, err := scope.NewSourceWire(request.ProjectID, request.ActorID, request.ApplicationID, request.VersionID, request.VersionDetails)
	if err != nil {
		return scope.SourceReference{}, err
	}
	owner.source, err = scope.DecodeSourceWire(wire.CanonicalBytes(), request.VersionDetails, wire.Reference(), request.ProjectID, request.ActorID)
	return wire.Reference(), err
}
func (owner *originalSourceOwnerFixture) RestoreOriginalRootSource(_ context.Context, request RootSourceRestoreRequest) (scope.SourceDefinition, error) {
	owner.restores = append(owner.restores, request)
	if owner.events != nil {
		*owner.events = append(*owner.events, "restore")
	}
	return owner.source, owner.err
}

func (owner *originalSourceOwnerFixture) RestoreOriginalRootContinuation(ctx context.Context, request RootSourceRestoreRequest) (RootSourceContinuation, error) {
	source, err := owner.RestoreOriginalRootSource(ctx, request)
	tools := owner.applicationTools
	if len(tools) == 0 {
		tools = json.RawMessage(`[]`)
	}
	return RootSourceContinuation{Source: source, ApplicationTools: bytes.Clone(tools)}, err
}

type sourceRecordingFreezer struct {
	calls  []CurrentApplicationVersionFreezeRequest
	events *[]string
}

func (freezer *sourceRecordingFreezer) FreezeCurrentApplicationVersion(_ context.Context, request CurrentApplicationVersionFreezeRequest) (json.RawMessage, error) {
	request.VersionDetails = bytes.Clone(request.VersionDetails)
	freezer.calls = append(freezer.calls, request)
	if freezer.events != nil {
		*freezer.events = append(*freezer.events, "freeze")
	}
	var fields map[string]json.RawMessage
	frozen, err := (&currentApplicationVersionFreezerStub{}).FreezeCurrentApplicationVersion(context.Background(), request)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(frozen, &fields); err != nil {
		return nil, err
	}
	fields["runtime_provider_materialized"] = json.RawMessage(`true`)
	return json.Marshal(fields)
}
func rootSourceTarget() CurrentApplicationTarget {
	raw := json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Original declaration","llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},"meta":{},"tools":[],"skills":[]}`)
	runtime := bytes.Replace(raw, []byte("Original declaration"), []byte("Original declaration with nested runtime skills"), 1)
	return CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41, Variables: json.RawMessage(`[]`), VersionDetails: runtime, SourceVersionDetails: raw, ChatHistory: json.RawMessage(`[]`)}
}
func sourceService(t *testing.T, resolver *currentApplicationResolverStub, owner OriginalRootSourceOwner, freezer CurrentApplicationVersionFreezer) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithOriginalSourceDefinitions(owner), admissions
}
func submittedSource(t *testing.T, input *runtimeAgentInput) (scope.SourceReference, map[string]json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(input.Application, &fields) != nil {
		t.Fatal("malformed application")
	}
	var ref scope.SourceReference
	if json.Unmarshal(fields["source_definition"], &ref) != nil || ref.Validate() != nil {
		t.Fatal("protected source reference absent")
	}
	return ref, fields
}
func TestOriginalSourceStartCapturesAdmittedRawBeforeFreezeAndRuntimeAdditions(t *testing.T) {
	target := rootSourceTarget()
	events := []string{}
	owner := &originalSourceOwnerFixture{events: &events}
	freezer := &sourceRecordingFreezer{events: &events}
	service, admissions := sourceService(t, &currentApplicationResolverStub{target: target}, owner, freezer)
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	if len(owner.captures) != 1 || len(admissions.requests) != 1 || len(events) != 2 || events[0] != "capture" || events[1] != "freeze" {
		t.Fatalf("events=%v admissions=%d", events, len(admissions.requests))
	}
	if !bytes.Equal(owner.captures[0].VersionDetails, target.SourceVersionDetails) || owner.captures[0].ProjectID != 7 || owner.captures[0].ActorID != 11 {
		t.Fatal("raw admitted source was replaced by runtime projection")
	}
	ref, fields := submittedSource(t, admissions.requests[0].Input)
	if ref != owner.source.Reference || bytes.Contains(fields["version_details"], []byte(`"source_version_details"`)) || !bytes.Contains(fields["version_details"], []byte("nested runtime skills")) {
		t.Fatal("source carrier leaked or runtime version was replaced")
	}
	// Real input projection adds memories/context after source capture, retaining ref.
	target.OriginalSource = &ref
	projected, err := currentApplicationInput(validCurrentApplicationStartRequest(), target, json.RawMessage(`{}`), json.RawMessage(`{}`), nil, "memory runtime text", "project runtime text")
	if err != nil {
		t.Fatal(err)
	}
	same, _ := submittedSource(t, projected)
	if same != ref || bytes.Contains(owner.source.PreRedemptionVersion, []byte("memory runtime text")) || bytes.Contains(owner.source.PreRedemptionVersion, []byte("project runtime text")) {
		t.Fatal("runtime text became original source")
	}
}
func TestOriginalSourceNilOwnerPreservesInputAndOverwritesForgedMarker(t *testing.T) {
	target := rootSourceTarget()
	forged := scope.SourceReference{SchemaVersion: scope.SourceSchema, SourceID: scope.Digest([]byte("forged")), Revision: 1, DigestSHA256: scope.Digest([]byte("fake")), SourceDefinitionSHA256: scope.Digest([]byte("fake-source")), YAMLSHA256: scope.Digest([]byte("fake-yaml")), ApplicationID: 31, VersionID: 41, Kind: "saved_application"}
	run := func(marker *scope.SourceReference) []byte {
		copy := target
		copy.OriginalSource = marker
		s, admissions := sourceService(t, &currentApplicationResolverStub{target: copy}, nil, &currentApplicationVersionFreezerStub{})
		if _, err := s.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
			t.Fatal(err)
		}
		return admissions.requests[0].Input.Application
	}
	if !bytes.Equal(run(nil), run(&forged)) {
		t.Fatal("disabled producer changed old application bytes")
	}
	target.OriginalSource = &forged
	owner := &originalSourceOwnerFixture{}
	service, admissions := sourceService(t, &currentApplicationResolverStub{target: target}, owner, &sourceRecordingFreezer{})
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	ref, _ := submittedSource(t, admissions.requests[0].Input)
	if ref == forged || ref != owner.source.Reference {
		t.Fatal("editable source marker conferred authority")
	}
}
func TestOriginalSourceEnabledProducerRefusesMissingRawBeforeCredentials(t *testing.T) {
	target := rootSourceTarget()
	target.SourceVersionDetails = nil
	owner := &originalSourceOwnerFixture{}
	freezer := &sourceRecordingFreezer{}
	service, admissions := sourceService(t, &currentApplicationResolverStub{target: target}, owner, freezer)
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); !errors.Is(err, ErrUnsupportedCurrentAgentStart) || len(freezer.calls) != 0 || len(admissions.requests) != 0 {
		t.Fatalf("err=%v freeze=%d writes=%d", err, len(freezer.calls), len(admissions.requests))
	}
}
func TestOriginalSourceEphemeralAdhocStartCapturesMainResolvedDeclaration(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{TargetParticipantID: 21, LLMSettings: json.RawMessage(`{"model_name":"test","model_project_id":7,"openai_compatible":false}`), Instructions: "Unsaved ephemeral declaration", Tools: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`), ConversationMeta: json.RawMessage(`{}`)}}
	owner := &originalSourceOwnerFixture{}
	service, admissions := sourceService(t, resolver, owner, &sourceRecordingFreezer{})
	if _, err := service.StartCurrentAdhoc(t.Context(), validCurrentAdhocStartRequest()); err != nil {
		t.Fatal(err)
	}
	ref, _ := submittedSource(t, admissions.requests[0].Input)
	if ref.Kind != "ephemeral_definition" || ref.ApplicationID != 0 || ref.VersionID != 0 || owner.source.Instructions != resolver.adhocTarget.Instructions || bytes.Contains(owner.source.PreRedemptionVersion, []byte("runtime_provider_materialized")) {
		t.Fatal("ephemeral source was not the Main-resolved declaration")
	}
	// This exercises the admitted ad hoc producer, not an unsaved editor UI route.
}
func sourceContinueFixture() (*currentApplicationResolverStub, CurrentContinuationRequest) {
	resolver := &currentApplicationResolverStub{target: rootSourceTarget(), continuationTarget: CurrentContinuationTarget{Kind: CurrentRegenerationApplication, TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "original question", ThreadID: "thread-original-source", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010", InterruptID: "source-review", AvailableActions: []string{"approve"}, HITLInterrupts: []CurrentHITLInterrupt{{InterruptID: "source-review", AvailableActions: []string{"approve"}}}}}
	request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", ThreadID: "thread-original-source", Action: "approve"}
	return resolver, request
}
func TestOriginalSourceContinueRestoresExactOriginalAndRegenerateCapturesNewSource(t *testing.T) {
	resolver, request := sourceContinueFixture()
	owner := &originalSourceOwnerFixture{}
	if _, err := owner.CaptureOriginalRootSource(t.Context(), RootSourceCaptureRequest{7, 11, 31, 41, resolver.target.SourceVersionDetails}); err != nil {
		t.Fatal(err)
	}
	original := owner.source
	owner.captures = nil
	resolver.target.VersionDetails = bytes.Replace(resolver.target.VersionDetails, []byte("Original declaration"), []byte("Changed current declaration"), 1)
	resolver.target.SourceVersionDetails = bytes.Replace(resolver.target.SourceVersionDetails, []byte("Original declaration"), []byte("Changed current declaration"), 1)
	freezer := &sourceRecordingFreezer{}
	service, admissions := sourceService(t, resolver, owner, freezer)
	if _, err := service.ContinueCurrentAgent(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	ref, _ := submittedSource(t, admissions.requests[0].Input)
	if ref != original.Reference || len(owner.captures) != 0 || len(owner.restores) != 1 || !bytes.Equal(freezer.calls[0].VersionDetails, original.PreRedemptionVersion) || owner.restores[0].ExecutionGeneration != resolver.continuationTarget.ExecutionGeneration {
		t.Fatal("continue recaptured an edited source")
	}
	regeneration := validCurrentRegenerationRequest()
	resolver.regenerationTarget = CurrentRegenerationTarget{Kind: CurrentRegenerationApplication, ConversationUUID: regeneration.ConversationUUID, TargetParticipantID: 21, QuestionID: regeneration.QuestionID, UserInput: "original question"}
	if _, err := service.RegenerateCurrentAgent(t.Context(), regeneration); err != nil {
		t.Fatal(err)
	}
	fresh, _ := submittedSource(t, admissions.requests[1].Input)
	if fresh == original.Reference || len(owner.captures) != 1 || len(owner.restores) != 1 || !bytes.Equal(owner.captures[0].VersionDetails, resolver.target.SourceVersionDetails) {
		t.Fatal("new regeneration reused prior source")
	}
}
func TestOriginalSourceContinueRefusesForeignActorProjectReferenceAndDrift(t *testing.T) {
	for _, name := range []string{"actor", "project", "reference", "bytes", "yaml", "missing"} {
		t.Run(name, func(t *testing.T) {
			resolver, request := sourceContinueFixture()
			owner := &originalSourceOwnerFixture{}
			if _, err := owner.CaptureOriginalRootSource(t.Context(), RootSourceCaptureRequest{7, 11, 31, 41, resolver.target.SourceVersionDetails}); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "actor":
				owner.source.ActorID = 12
			case "project":
				owner.source.ResourceProjectID = 8
			case "reference":
				owner.source.Reference.SourceID = scope.Digest([]byte("other"))
			case "bytes":
				owner.source.PreRedemptionVersion = bytes.Replace(owner.source.PreRedemptionVersion, []byte("Original"), []byte("Changed"), 1)
			case "yaml":
				owner.source.Instructions = "changed"
			case "missing":
				owner.err = scope.ErrDenied
			}
			freezer := &sourceRecordingFreezer{}
			service, admissions := sourceService(t, resolver, owner, freezer)
			if _, err := service.ContinueCurrentAgent(t.Context(), request); !errors.Is(err, ErrUnsupportedCurrentAgentStart) || len(freezer.calls) != 0 || len(admissions.requests) != 0 {
				t.Fatalf("err=%v freeze=%d writes=%d", err, len(freezer.calls), len(admissions.requests))
			}
		})
	}
}

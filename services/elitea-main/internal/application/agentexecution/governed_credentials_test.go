package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	frozenBinding   = "0192f0aa-0000-7000-8000-000000000001"
	frozenReference = "0192f0aa-0000-7000-8000-0000000000c1"
	frozenTarget    = "0192f0aa-0000-7000-8000-0000000000d1"
)

type fakeGovernedBindings struct {
	id    string
	calls int
}

func (f *fakeGovernedBindings) UsableBindingForVersion(_ context.Context, tenant, version int64) (string, bool, error) {
	f.calls++
	if tenant != 7 || version != 4 {
		return "", false, errors.New("unexpected binding lookup")
	}
	return f.id, f.id != "", nil
}

type fakeGovernedToolkits map[int64]map[string]any

func (f fakeGovernedToolkits) StoredToolkit(_ context.Context, project, id int64) (string, map[string]any, bool, error) {
	if project != 7 {
		return "", nil, false, errors.New("unexpected project")
	}
	settings, ok := f[id]
	return "jira", settings, ok, nil
}

// fakeGovernedResolver resolves frozenReference for any bound toolkit at the
// digest of its stored settings; bound lists the bound toolkits.
type fakeGovernedResolver struct {
	toolkits fakeGovernedToolkits
	bound    map[int64]bool
	governed []bool
	revoked  bool
}

func (f *fakeGovernedResolver) digest(id int64) [32]byte {
	digest, _ := credentialgrant.ToolkitDigest("jira", f.toolkits[id])
	return digest
}

func (f *fakeGovernedResolver) Resolve(ctx context.Context, _ int64, binding string, toolkit int64, digest [32]byte,
	ref configurationapp.ToolkitCredentialRef) (configurationapp.GovernedResolution, error) {
	f.governed = append(f.governed, configurationapp.IsGovernedRun(ctx))
	switch {
	case ref.Private:
		return configurationapp.GovernedResolution{}, configurationapp.ErrPrivateCredentialNotGoverned
	case ref.CredentialReferenceID == "":
		return configurationapp.GovernedResolution{}, configurationapp.ErrGovernedReferenceRequired
	case !f.bound[toolkit]:
		return configurationapp.GovernedResolution{}, configurationapp.ErrGovernedToolkitNotBound
	case digest != f.digest(toolkit):
		return configurationapp.GovernedResolution{}, configurationapp.ErrGovernedToolkitDigestMismatch
	case binding != frozenBinding || f.revoked:
		return configurationapp.GovernedResolution{}, configurationapp.ErrGovernedGrantMissing
	}
	return configurationapp.GovernedResolution{ToolkitID: toolkit, CredentialReferenceID: ref.CredentialReferenceID,
		TargetBindingID: frozenTarget, Generation: 2}, nil
}

func (f *fakeGovernedResolver) BindToolkit(_ context.Context, _ int64, _ string, toolkit int64, digest [32]byte) (bool, error) {
	if !f.bound[toolkit] {
		return false, nil
	}
	if digest != f.digest(toolkit) {
		return false, configurationapp.ErrGovernedToolkitDigestMismatch
	}
	return true, nil
}

// fakeAdmissionReplays answers one committed admission when found.
type fakeAdmissionReplays struct {
	outcome executionapp.AdmissionOutcome
	digest  runtimedomain.Digest
	found   bool
	calls   int
}

func (f *fakeAdmissionReplays) AgentAdmissionReplay(_ context.Context, scope, key string) (executionapp.AdmissionOutcome, runtimedomain.Digest, bool, error) {
	f.calls++
	if scope != "tenant/7/19" || key != "message-1" {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, false, errors.New("unexpected replay lookup")
	}
	return f.outcome, f.digest, f.found, nil
}

func governedWorld(t *testing.T, stored fakeGovernedToolkits, bound ...int64) (*GovernedCredentials, *fakeGovernedResolver, *fakeGovernedBindings) {
	credentials, resolver, bindings, _ := governedWorldWithReplays(t, stored, bound...)
	return credentials, resolver, bindings
}

func governedWorldWithReplays(t *testing.T, stored fakeGovernedToolkits, bound ...int64) (*GovernedCredentials, *fakeGovernedResolver, *fakeGovernedBindings, *fakeAdmissionReplays) {
	t.Helper()
	resolver := &fakeGovernedResolver{toolkits: stored, bound: map[int64]bool{}}
	for _, id := range bound {
		resolver.bound[id] = true
	}
	bindings := &fakeGovernedBindings{id: frozenBinding}
	replays := &fakeAdmissionReplays{}
	credentials, err := NewGovernedCredentials(resolver, bindings, stored, replays)
	if err != nil {
		t.Fatal(err)
	}
	return credentials, resolver, bindings, replays
}

func jiraRef(fields map[string]any) map[string]any {
	return map[string]any{"jira_configuration": fields}
}

func governedSubmit(t *testing.T, toolkitIDs ...int64) SubmitRequest {
	t.Helper()
	tools := []any{map[string]any{"type": "memory", "id": 99, "settings": map[string]any{}}}
	for _, id := range toolkitIDs {
		tools = append(tools, map[string]any{"type": "jira", "id": id, "name": "jira",
			"settings": map[string]any{"jira_configuration": map[string]any{"base_url": "https://jira.example.test"}}})
	}
	application, err := json.Marshal(map[string]any{"id": 3, "version_details": map[string]any{"id": 4, "tools": tools}})
	if err != nil {
		t.Fatal(err)
	}
	const (
		question     = "ee92ccbd-3312-4c72-b20b-fddf224e7c0e"
		conversation = "8bc66e50-46c4-4e2c-94ec-daec6c596ac0"
		response     = "061e2c58-2e09-5853-a006-532b082a0433"
	)
	input := validAgentInput()
	input.Application = application
	input.ExecutionGeneration = proto.String(question)
	return SubmitRequest{
		Identity:        executionapp.AdmissionIdentity{TenantID: "tenant", ResourceProjectID: "7", ProjectionProjectID: "7", ActorID: "19"},
		IdempotencyKey:  "message-1",
		CapabilityID:    executiondomain.AgentApplicationCapability,
		ClientStreamID:  conversation,
		ClientMessageID: response,
		SIOEvent:        "chat_predict",
		Input:           input,
		CurrentTurn: &CurrentApplicationTurn{
			ProjectID: 7, ActorUserID: 19, ConversationUUID: conversation, TargetParticipantID: 21,
			ApplicationID: 3, ApplicationVersionID: 4, QuestionID: question,
			QuestionItemID: "3d3f33b7-e629-4db7-b4ee-c2914067897a", ResponseMessageID: response,
			QuestionMeta: []byte(`{}`), UserInput: "hello",
		},
	}
}

func frozenToolSettings(t *testing.T, input *runtimev1.AgentExecutionInputV1) map[int64]map[string]any {
	t.Helper()
	var application struct {
		VersionDetails struct {
			Tools []struct {
				ID       int64          `json:"id"`
				Settings map[string]any `json:"settings"`
			} `json:"tools"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
		t.Fatal(err)
	}
	out := map[int64]map[string]any{}
	for _, tool := range application.VersionDetails.Tools {
		out[tool.ID] = tool.Settings
	}
	return out
}

// Acceptance (#1119): a bound agent's credentialed toolkits resolve by exact
// reference; each reference is written into its toolkit's frozen settings,
// where the worker reads it, and the list is returned for the security
// context, toolkit ids ascending. A bound toolkit with no configuration
// reference is frozen at its digest alone; an unbound one is not frozen.
func TestGovernedFreezeWritesTheExactReferences(t *testing.T) {
	stored := fakeGovernedToolkits{
		31: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference}),
		21: jiraRef(map[string]any{"elitea_title": "jira-dev", "private": false, "credential_reference_id": frozenReference}),
		41: {"base_url": "https://sharepoint.example.test"},
		51: {"limit": 1.0},
	}
	credentials, resolver, _ := governedWorld(t, stored, 21, 31, 41)
	request := governedSubmit(t, 31, 21, 41, 51)
	original := append([]byte(nil), request.Input.GetApplication()...)

	input, freeze, err := credentials.Freeze(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(request.Input.GetApplication()) != string(original) {
		t.Fatal("the caller's input was modified")
	}
	settings := frozenToolSettings(t, input)
	for _, id := range []int64{21, 31} {
		if settings[id]["credential_reference_id"] != frozenReference ||
			settings[id]["jira_configuration"].(map[string]any)["base_url"] != "https://jira.example.test" {
			t.Fatalf("toolkit %d settings %v", id, settings[id])
		}
	}
	for _, id := range []int64{41, 51, 99} {
		if _, ok := settings[id]["credential_reference_id"]; ok {
			t.Fatalf("toolkit %d got a reference: %v", id, settings[id])
		}
	}
	if freeze == nil || freeze.AgentBindingID != frozenBinding || freeze.ApplicationID != 3 || freeze.ApplicationVersionID != 4 {
		t.Fatalf("freeze %+v", freeze)
	}
	want := []GovernedCredentialReference{
		{ToolkitID: 21, CredentialReferenceID: frozenReference, TargetBindingID: frozenTarget, Generation: 2, ToolkitDigest: resolver.digest(21)},
		{ToolkitID: 31, CredentialReferenceID: frozenReference, TargetBindingID: frozenTarget, Generation: 2, ToolkitDigest: resolver.digest(31)},
		{ToolkitID: 41, ToolkitDigest: resolver.digest(41)},
	}
	if len(freeze.References) != len(want) {
		t.Fatalf("references %+v", freeze.References)
	}
	for i := range want {
		if freeze.References[i] != want[i] {
			t.Fatalf("reference %d = %+v, want %+v", i, freeze.References[i], want[i])
		}
	}
	for _, governed := range resolver.governed {
		if !governed {
			t.Fatal("the resolution did not run under the governed mark")
		}
	}
}

// Acceptance: a title-only reference, a private reference (D1) or an
// unbound credentialed toolkit refuses the admission with its typed error,
// and Submit stores nothing.
func TestGovernedFreezeRefusesWithTypedErrors(t *testing.T) {
	for name, c := range map[string]struct {
		ref   map[string]any
		bound bool
		want  error
		code  string
	}{
		"title only": {map[string]any{"elitea_title": "jira-prod", "private": false}, true,
			configurationapp.ErrGovernedReferenceRequired, "credential_reference_required"},
		"private": {map[string]any{"elitea_title": "mine", "private": true, "credential_reference_id": frozenReference}, true,
			configurationapp.ErrPrivateCredentialNotGoverned, "private_credential_not_governed"},
		"unbound toolkit": {map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference}, false,
			configurationapp.ErrGovernedToolkitNotBound, "toolkit_not_bound"},
	} {
		t.Run(name, func(t *testing.T) {
			bound := []int64{}
			if c.bound {
				bound = append(bound, 21)
			}
			credentials, _, _ := governedWorld(t, fakeGovernedToolkits{21: jiraRef(c.ref)}, bound...)
			store := successfulRecordingStore()
			service := testAdmissionService(t, store).WithGovernedCredentials(credentials)
			_, err := service.Submit(context.Background(), governedSubmit(t, 21))
			var refused *GovernedCredentialAdmissionError
			if !errors.Is(err, c.want) || !errors.As(err, &refused) || refused.Code != c.code || refused.ToolkitID != 21 {
				t.Fatalf("err = %v", err)
			}
			if store.admission.Record.Job.ID != "" {
				t.Fatal("a refused admission reached the store")
			}
		})
	}
}

// A retry of a committed admission (same idempotency key) replays it before
// anything is resolved, so a grant revoked since does not refuse it. The
// request digest is the client's request, not the frozen input: a governed
// admission digests exactly as an ungoverned one of the same request.
func TestGovernedRetryReplaysTheCommittedAdmission(t *testing.T) {
	stored := fakeGovernedToolkits{21: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false, "credential_reference_id": frozenReference})}
	credentials, resolver, _, replays := governedWorldWithReplays(t, stored, 21)
	store := successfulRecordingStore()
	service := testAdmissionService(t, store).WithGovernedCredentials(credentials)
	first, err := service.Submit(context.Background(), governedSubmit(t, 21))
	if err != nil {
		t.Fatal(err)
	}
	if store.admission.GovernedCredentials == nil || len(store.admission.GovernedCredentials.References) != 1 {
		t.Fatalf("freeze %+v", store.admission.GovernedCredentials)
	}
	committed := store.admission.Record.RequestDigest
	plain := successfulRecordingStore()
	if _, err := testAdmissionService(t, plain).Submit(context.Background(), governedSubmit(t, 21)); err != nil {
		t.Fatal(err)
	}
	if plain.admission.Record.RequestDigest != committed {
		t.Fatal("the request digest covers the frozen input, not the client's request")
	}
	if string(plain.admission.Record.InputBundle.Entries[0].Content) == string(store.admission.Record.InputBundle.Entries[0].Content) {
		t.Fatal("the governed bundle does not carry the frozen reference")
	}

	resolver.revoked = true
	replays.outcome, replays.digest, replays.found = first, committed, true
	retryStore := successfulRecordingStore()
	retry := testAdmissionService(t, retryStore).WithGovernedCredentials(credentials)
	calls := len(resolver.governed)
	outcome, err := retry.Submit(context.Background(), governedSubmit(t, 21))
	if err != nil {
		t.Fatalf("replay after revocation: %v", err)
	}
	if outcome.ExecutionID != first.ExecutionID || outcome.CommandID != first.CommandID || outcome.Created {
		t.Fatalf("replay outcome %+v, want the original %+v", outcome, first)
	}
	if len(resolver.governed) != calls || retryStore.admission.Record.Job.ID != "" {
		t.Fatal("a replay was resolved or reached the store again")
	}

	// A different request under the same key is a conflict, not a freeze.
	changed := governedSubmit(t, 21)
	changed.Input.UserInput = []byte(`"other"`)
	if _, err := retry.Submit(context.Background(), changed); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if len(resolver.governed) != calls {
		t.Fatal("a conflicting retry was resolved")
	}
}

// A malformed request is refused before any governed read.
func TestGovernedAdmissionValidatesBeforeReading(t *testing.T) {
	credentials, resolver, bindings, replays := governedWorldWithReplays(t, fakeGovernedToolkits{}, 21)
	service := testAdmissionService(t, successfulRecordingStore()).WithGovernedCredentials(credentials)
	request := governedSubmit(t, 21)
	request.CurrentTurn.QuestionID = "3d3f33b7-e629-4db7-b4ee-c2914067897b"
	if _, err := service.Submit(context.Background(), request); !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("err = %v", err)
	}
	if replays.calls != 0 || bindings.calls != 0 || len(resolver.governed) != 0 {
		t.Fatal("a malformed request was read before it was refused")
	}
}

// A toolkit frozen at its digest alone leaves the input untouched: the same
// message and the same bytes.
func TestGovernedFreezeOfDigestOnlyToolkitsKeepsTheInputBytes(t *testing.T) {
	stored := fakeGovernedToolkits{41: {"base_url": "https://sharepoint.example.test"}}
	credentials, _, _ := governedWorld(t, stored, 41)
	request := governedSubmit(t, 41)
	original := append([]byte(nil), request.Input.GetApplication()...)
	input, freeze, err := credentials.Freeze(context.Background(), request)
	if err != nil || freeze == nil || len(freeze.References) != 1 || freeze.References[0].CredentialReferenceID != "" {
		t.Fatalf("freeze %+v err=%v", freeze, err)
	}
	if input != request.Input || string(input.GetApplication()) != string(original) {
		t.Fatal("a digest-only freeze rewrote the input")
	}
}

func TestGovernedFreezeRefusesTwoReferencesInOneToolkit(t *testing.T) {
	stored := fakeGovernedToolkits{21: {
		"jira_configuration": map[string]any{"elitea_title": "a", "private": false, "credential_reference_id": frozenReference},
		"other_configuration": map[string]any{"elitea_title": "b", "private": false, "credential_reference_id": frozenReference},
	}}
	credentials, _, _ := governedWorld(t, stored, 21)
	if _, _, err := credentials.Freeze(context.Background(), governedSubmit(t, 21)); !errors.Is(err, configurationapp.ErrGovernedReferenceAmbiguous) {
		t.Fatalf("err = %v", err)
	}
}

// A version with no usable binding is not a governed agent: the input is
// unchanged, and the freeze names no binding so the admission transaction
// refuses one that appeared in between. An ad-hoc run reads nothing.
func TestGovernedFreezeOfAnUnboundOrAdhocRun(t *testing.T) {
	stored := fakeGovernedToolkits{21: jiraRef(map[string]any{"elitea_title": "jira-prod", "private": false})}
	credentials, _, bindings := governedWorld(t, stored, 21)
	bindings.id = ""
	request := governedSubmit(t, 21)
	input, freeze, err := credentials.Freeze(context.Background(), request)
	if err != nil || input != request.Input || freeze == nil || freeze.AgentBindingID != "" || len(freeze.References) != 0 ||
		freeze.ApplicationVersionID != 4 {
		t.Fatalf("unbound: freeze %+v err=%v", freeze, err)
	}

	adhoc := governedSubmit(t, 21)
	adhoc.CapabilityID, adhoc.CurrentTurn = executiondomain.AgentAdhocCapability, nil
	calls := bindings.calls
	input, freeze, err = credentials.Freeze(context.Background(), adhoc)
	if err != nil || input != adhoc.Input || freeze != nil || bindings.calls != calls {
		t.Fatalf("adhoc: freeze %+v err=%v", freeze, err)
	}
}

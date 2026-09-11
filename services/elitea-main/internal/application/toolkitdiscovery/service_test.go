package toolkitdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	call "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"google.golang.org/protobuf/proto"
)

type savedReader struct {
	found bool
	seen  Request
}

func (r *savedReader) GetCurrentToolkit(_ context.Context, p, a, k int32) (indexing.CurrentToolkitSnapshot, bool, error) {
	r.seen = Request{int64(p), int64(a), int64(k)}
	return indexing.CurrentToolkitSnapshot{ID: k, Type: "github", Name: "saved", Settings: map[string]any{"project": "saved-project"}}, r.found, nil
}

type referenceSettings struct{ calls int }

func (r *referenceSettings) Resolve(_ context.Context, q configs.CurrentToolkitSettingsRequest) (map[string]any, error) {
	r.calls++
	if q.Mode != configs.CurrentToolkitSettingsReferenceMode {
		return nil, errors.New("secret redemption before claim")
	}
	return q.Settings, nil
}

type policySource struct{ err error }

func (p policySource) ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error) {
	return guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"delete_file"}}}), p.err
}
func TestSavedDiscoveryUsesActorAndReferenceSettings(t *testing.T) {
	reader := &savedReader{found: true}
	settings := &referenceSettings{}
	resolver, err := NewCurrentAuthoritativeInputResolver(reader, settings, policySource{})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{7, 42, 19}
	inputs, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if reader.seen != request || settings.calls != 1 || string(inputs.Settings) != `{"project":"saved-project"}` || !validDiscoveryRuntimeContext(inputs.RuntimeContext) {
		t.Fatalf("wrong authority: %+v %+v", reader, inputs)
	}
	reader.found = false
	_, err = resolver.Resolve(context.Background(), request)
	if !errors.Is(err, call.ErrToolkitNotVisible) || settings.calls != 1 {
		t.Fatal("invisible toolkit reached settings")
	}
	resolver, _ = NewCurrentAuthoritativeInputResolver(reader, settings, policySource{errors.New("policy unavailable")})
	reader.found = true
	if _, err = resolver.Resolve(context.Background(), request); err == nil {
		t.Fatal("failed policy admitted discovery")
	}
}
func inputsFixture() AuthoritativeInputs {
	return AuthoritativeInputs{ToolkitID: 19, ToolkitType: "github", Settings: json.RawMessage(`{"selected_tools":["list_issues"]}`), RuntimeContext: json.RawMessage(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}}}`)}
}
func ids() func() (string, error) {
	n := 0
	return func() (string, error) { n++; return fmt.Sprintf("id-%d", n), nil }
}
func TestDiscoveryBundleBindsExactSettingsAndPolicy(t *testing.T) {
	factory, err := NewInputBundleFactory(InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "runtime"}, ids())
	if err != nil {
		t.Fatal(err)
	}
	inputs := inputsFixture()
	bundle, binding, err := factory.Build(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Entries) != 2 || bundle.Entries[0].SemanticRole != domain.ToolkitAvailableToolsSettingsRole || bundle.Entries[1].SemanticRole != "toolkit.available_tools.runtime_context" {
		t.Fatal("invalid role binding")
	}
	inputs.Settings[0] = '!'
	if !json.Valid(bundle.Entries[0].Content) {
		t.Fatal("source aliases immutable bundle")
	}
	bad := bundle.Clone()
	bad.Entries[1].SemanticRole = "toolkit.call_tool.arguments"
	if binding.Validate(bad) == nil {
		t.Fatal("wrong role accepted")
	}
	badInput := inputsFixture()
	badInput.RuntimeContext = json.RawMessage(`{}`)
	if _, _, err := factory.Build(context.Background(), badInput); err == nil {
		t.Fatal("implicit empty policy accepted")
	}
	first := SubmitRequest{Identity: executionapp.AdmissionIdentity{TenantID: "7", ResourceProjectID: "7", ProjectionProjectID: "7", ActorID: "42"}, Inputs: inputsFixture()}
	digest := discoveryRequestDigest(first, binding, bundle)
	bad = bundle.Clone()
	bad.Entries[1].Content = []byte(`{"toolkit_security":{"blocked_tools":{"github":["listissues"]}}}`)
	if discoveryRequestDigest(first, binding, bad) == digest {
		t.Fatal("changed policy reused admission digest")
	}
}

type admissionStub struct {
	calls    int
	admitted AdmittedRun
}

func (s *admissionStub) Submit(_ context.Context, r SubmitRequest) (AdmittedRun, error) {
	s.calls++
	return s.admitted, nil
}

type resolverStub struct{ err error }

func (s resolverStub) Resolve(context.Context, Request) (AuthoritativeInputs, error) {
	return inputsFixture(), s.err
}

type verdictStub bool

func (s verdictStub) SupportsToolkitType(string) (bool, string) { return bool(s), "" }

type dispatcherStub struct{ calls int }

func (s *dispatcherStub) Dispatch(context.Context, Dispatch) error { s.calls++; return nil }

type resultStore struct {
	settlement Settlement
	found      bool
	calls      int
}

func (s *resultStore) ReadToolkitAvailableToolsSettlement(context.Context, string, uint64) (Settlement, bool, error) {
	return s.settlement, s.found, nil
}
func (s *resultStore) ReadToolkitDiscoveryResult(_ context.Context, project int64, id string, generation uint64, ref *runtimev1.ToolkitAvailableToolsArtifactReferenceV1) ([]byte, error) {
	s.calls++
	if project != 7 || id != "execution" || generation != 1 || ref.ArtifactId != "artifact" {
		return nil, errors.New("wrong scope")
	}
	return []byte(`{"tools":[{"name":"list_issues","description":"List issues"}],"args_schemas":{"list_issues":{"type":"object"}}}`), nil
}
func TestDiscoveryWaitConsumesArtifactOnlyAfterSettlement(t *testing.T) {
	payload, _ := proto.Marshal(&runtimev1.ToolkitAvailableToolsResultV1{ResultArtifact: &runtimev1.ToolkitAvailableToolsArtifactReferenceV1{ArtifactId: "artifact"}})
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			store := &resultStore{found: true, settlement: Settlement{PayloadType: PayloadTypeToolkitAvailableToolsResult, Outcome: executionapp.SettlementSucceeded, Payload: payload}}
			if !success {
				store.settlement.PayloadType = "RUNTIME_FAILURE"
			}
			admissions := &admissionStub{admitted: AdmittedRun{Outcome: executionapp.AdmissionOutcome{ExecutionID: "execution", Created: false}}}
			dispatch := &dispatcherStub{}
			service, err := NewService(resolverStub{}, verdictStub(true), admissions, dispatch, store, store, DispatchPolicy{CapabilityVersion: "1", ResourceClass: "index", IsolationClass: "isolated", Priority: 1, LimitsRevision: "limits"}, ids(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.AvailableTools(context.Background(), Request{7, 42, 19})
			if success {
				if err != nil || len(result.Tools) != 1 || store.calls != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, ErrRuntimeFailure) || store.calls != 0 {
				t.Fatal("failed execution fetched artifact")
			}
			if dispatch.calls != 0 {
				t.Fatal("admission replay republished command")
			}
		})
	}
}

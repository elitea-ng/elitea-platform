package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

type pipelineApprovalStub struct {
	err      error
	calls    int
	project  int64
	app      int64
	version  json.RawMessage
	children PipelineChildVersions
}

func (s *pipelineApprovalStub) AdmitPipeline(_ context.Context, projectID, applicationID int64, version json.RawMessage, children PipelineChildVersions) error {
	s.calls++
	s.project, s.app, s.version, s.children = projectID, applicationID, version, children
	return s.err
}

// ADR-0041 decision 5: a pipeline the approval policy refuses does not start
// and nothing is admitted; the refusal keeps its typed code for the route.
func TestPipelineApprovalRefusesTheStart(t *testing.T) {
	refusal := approvalpolicy.CheckPipelineHITL(`
entry_point: post
nodes:
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`, []approvalpolicy.PipelineToolkit{{Name: "chat", Type: "slack"}}, agentlifecycle.TierHigh)
	if refusal == nil {
		t.Fatal("the fixture pipeline is admitted")
	}
	for name, test := range map[string]struct {
		err         error
		unsupported bool
	}{
		"admitted":    {nil, false},
		"refused":     {refusal, true},
		"source down": {errors.New("registry down"), false},
	} {
		t.Run(name, func(t *testing.T) {
			service, admissions, _ := contextDeliveryFixture(t, &contextPolicyStub{strategy: contextsettings.DefaultStrategy()})
			stub := &pipelineApprovalStub{err: test.err}
			service.WithPipelineApproval(stub, nil)
			_, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest())
			if stub.calls != 1 || stub.project != 7 || stub.app != 31 || len(stub.version) == 0 {
				t.Fatalf("check calls = %d, project = %d, app = %d", stub.calls, stub.project, stub.app)
			}
			if test.err == nil {
				if err != nil || len(admissions.requests) != 1 {
					t.Fatalf("an admitted pipeline did not start: %v", err)
				}
				return
			}
			if !errors.Is(err, test.err) || len(admissions.requests) != 0 {
				t.Fatalf("err = %v, admissions = %d", err, len(admissions.requests))
			}
			if errors.Is(err, ErrUnsupportedCurrentAgentStart) != test.unsupported {
				t.Fatalf("unsupported identity = %t, want %t", !test.unsupported, test.unsupported)
			}
			if test.unsupported && approvalpolicy.Refusal(err) == nil {
				t.Fatal("the refusal lost its code")
			}
		})
	}
}

// Review finding 1: every path that freezes a run's root version checks the
// rule — start, ad hoc start, both regenerate arms and both continuation
// arms — and a refusal admits nothing.
func TestPipelineApprovalCoversEveryAdmissionPath(t *testing.T) {
	refusal := approvalpolicy.CompositionRefusal("test")
	continuation := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11, ConversationUUID: validCurrentApplicationStartRequest().ConversationUUID,
		ResponseMessageID: validCurrentRegenerationRequest().ResponseMessageID, Kind: CurrentContinuationOutputLimit,
	}
	for name, test := range map[string]struct {
		app int64
		run func(*CurrentApplicationStartService, *currentApplicationResolverStub) error
	}{
		"start": {31, func(s *CurrentApplicationStartService, _ *currentApplicationResolverStub) error {
			_, err := s.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest())
			return err
		}},
		"ad hoc start": {0, func(s *CurrentApplicationStartService, _ *currentApplicationResolverStub) error {
			request := validCurrentAdhocStartRequest()
			request.LLMSettings = json.RawMessage(`{}`)
			_, err := s.StartCurrentAdhoc(t.Context(), request)
			return err
		}},
		"regenerate": {31, func(s *CurrentApplicationStartService, _ *currentApplicationResolverStub) error {
			_, err := s.RegenerateCurrentAgent(t.Context(), validCurrentRegenerationRequest())
			return err
		}},
		"regenerate ad hoc": {0, func(s *CurrentApplicationStartService, r *currentApplicationResolverStub) error {
			r.regenerationTarget.Kind = CurrentRegenerationAdhoc
			_, err := s.RegenerateCurrentAgent(t.Context(), validCurrentRegenerationRequest())
			return err
		}},
		"continue": {31, func(s *CurrentApplicationStartService, _ *currentApplicationResolverStub) error {
			_, err := s.ContinueCurrentAgent(t.Context(), continuation)
			return err
		}},
		"continue ad hoc": {0, func(s *CurrentApplicationStartService, r *currentApplicationResolverStub) error {
			r.continuationTarget.Kind = CurrentRegenerationAdhoc
			_, err := s.ContinueCurrentAgent(t.Context(), continuation)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			source := &contextPolicyStub{
				strategy: contextsettings.DefaultStrategy(),
				frozen:   FrozenContextPolicy{Settings: json.RawMessage(`{}`), TaskLLM: json.RawMessage(`{"kwargs":{"model":"model","model_project_id":7}}`)},
			}
			service, admissions, resolver := contextDeliveryFixture(t, source)
			stub := &pipelineApprovalStub{err: refusal}
			service.WithPipelineApproval(stub, nil)
			err := test.run(service, resolver)
			if stub.calls != 1 || stub.project != 7 || stub.app != test.app || len(stub.version) == 0 {
				t.Fatalf("check calls = %d, project = %d, app = %d (want %d): %v", stub.calls, stub.project, stub.app, test.app, err)
			}
			if !errors.Is(err, ErrUnsupportedCurrentAgentStart) || approvalpolicy.Refusal(err) == nil || len(admissions.requests) != 0 {
				t.Fatalf("err = %v, admissions = %d", err, len(admissions.requests))
			}
		})
	}
}

type pipelineChildSourceStub struct {
	reads   int
	project int64
	saved   json.RawMessage
}

func (s *pipelineChildSourceStub) ReadPipelineChildVersion(_ context.Context, projectID, applicationID, versionID int64) (json.RawMessage, error) {
	s.reads++
	s.project = projectID
	if applicationID != 32 || versionID != 42 {
		return nil, errors.New("not found")
	}
	return s.saved, nil
}

// Review finding 2: the check reads a saved child in the run's project and
// freezes it with the run's own freezer.
func TestPipelineApprovalFreezesTheChildItReads(t *testing.T) {
	service, _, _ := contextDeliveryFixture(t, &contextPolicyStub{strategy: contextsettings.DefaultStrategy()})
	stub := &pipelineApprovalStub{}
	children := &pipelineChildSourceStub{saved: json.RawMessage(`{"agent_type":"pipeline","instructions":"entry_point: a","llm_settings":{"model_name":"model","model_project_id":7},"tools":[]}`)}
	service.WithPipelineApproval(stub, children)
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	if stub.children == nil {
		t.Fatal("the check got no child reader")
	}
	frozen, err := stub.children(t.Context(), 32, 42)
	if err != nil || children.reads != 1 || children.project != 7 {
		t.Fatalf("child read: %v, reads = %d, project = %d", err, children.reads, children.project)
	}
	var version map[string]any
	if json.Unmarshal(frozen, &version) != nil || version["agent_type"] != "pipeline" || version["instructions"] != "entry_point: a" {
		t.Fatalf("frozen child = %s", frozen)
	}
	if _, err := stub.children(t.Context(), 32, 43); err == nil {
		t.Fatal("a missing child is read")
	}
}

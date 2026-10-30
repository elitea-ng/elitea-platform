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
	err     error
	calls   int
	project int64
	app     int64
	version json.RawMessage
}

func (s *pipelineApprovalStub) AdmitPipeline(_ context.Context, projectID, applicationID int64, version json.RawMessage) error {
	s.calls++
	s.project, s.app, s.version = projectID, applicationID, version
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
			service.WithPipelineApproval(stub)
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

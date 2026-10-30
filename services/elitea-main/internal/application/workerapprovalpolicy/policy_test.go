package workerapprovalpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type settingsStub struct {
	settings platformconfig.ApprovalPolicySettings
	err      error
}

func (s settingsStub) Get(context.Context) (platformconfig.ApprovalPolicySettings, error) {
	return s.settings, s.err
}

type inputStub map[string][]byte

func (s inputStub) InputEntries(_ context.Context, _ string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, id := range ids {
		if content, ok := s[id]; ok {
			out[id] = content
		}
	}
	return out, nil
}

type tierStub struct {
	tiers map[[2]int64]string
	err   error
	reads int
}

func (s *tierStub) AgentRiskTier(_ context.Context, projectID, applicationID int64) (string, error) {
	s.reads++
	return s.tiers[[2]int64{projectID, applicationID}], s.err
}

func agentInput(t *testing.T, application string) []byte {
	t.Helper()
	content, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Application: []byte(application)})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func agentCommand() *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		ResourceProjectId: "17",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "request"},
		},
	}
}

func saved() platformconfig.ApprovalPolicySettings {
	settings := platformconfig.DefaultApprovalPolicySettings()
	settings.Stored = true
	return settings
}

// The rollout gate: an unsaved section answers nil and reads no registry.
func TestResolverIsAbsentUntilSaved(t *testing.T) {
	t.Parallel()
	tiers := &tierStub{tiers: map[[2]int64]string{{17, 5}: "high"}}
	resolver, err := NewResolver(settingsStub{settings: platformconfig.DefaultApprovalPolicySettings()},
		inputStub{"request": agentInput(t, `{"id":5}`)}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := resolver.WorkerApprovalPolicy(context.Background(), agentCommand())
	if err != nil || policy != nil || tiers.reads != 0 {
		t.Fatalf("policy = %v, err = %v, reads = %d", policy, err, tiers.reads)
	}
}

func TestResolverCarriesTheRootTier(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		application string
		tier        string
		want        string
		categories  []string
	}{
		"high agent":      {`{"id":5}`, "high", "high", []string{"delete", "external_send", "payment"}},
		"no registry row": {`{"id":5}`, "", "low", []string{}},
		"ad hoc":          {`{}`, "critical", "low", []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tiers := &tierStub{tiers: map[[2]int64]string{{17, 5}: test.tier}}
			resolver, err := NewResolver(settingsStub{settings: saved()}, inputStub{"request": agentInput(t, test.application)}, tiers)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := resolver.WorkerApprovalPolicy(context.Background(), agentCommand())
			if err != nil {
				t.Fatal(err)
			}
			if policy.GetRiskTier() != test.want || !slices.Equal(policy.GetApprovalCategories(), test.categories) {
				t.Fatalf("policy = %v", policy)
			}
		})
	}
}

func TestResolverFailsClosed(t *testing.T) {
	t.Parallel()
	input := inputStub{"request": agentInput(t, `{"id":5}`)}
	for name, test := range map[string]struct {
		settings settingsStub
		tiers    *tierStub
	}{
		"settings down": {settingsStub{err: errors.New("down")}, &tierStub{}},
		"registry down": {settingsStub{settings: saved()}, &tierStub{err: errors.New("down")}},
		"unknown tier":  {settingsStub{settings: saved()}, &tierStub{tiers: map[[2]int64]string{{17, 5}: "extreme"}}},
	} {
		resolver, err := NewResolver(test.settings, input, test.tiers)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.WorkerApprovalPolicy(context.Background(), agentCommand()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := NewResolver(nil, input, &tierStub{}); err == nil {
		t.Error("a resolver without settings was built")
	}
}

func TestPolicyUsesTheAdministratorMatrix(t *testing.T) {
	t.Parallel()
	settings := saved()
	settings.Policy.Categories[agentlifecycle.TierLow] = []toolcategory.Category{toolcategory.CodeExecution}
	policy := Policy(settings, agentlifecycle.TierLow)
	if !slices.Equal(policy.GetApprovalCategories(), []string{"code_execution"}) {
		t.Fatalf("policy = %v", policy)
	}
	if Policy(platformconfig.DefaultApprovalPolicySettings(), agentlifecycle.TierHigh) != nil {
		t.Fatal("an unsaved section has a policy")
	}
}

func frozen(t *testing.T, agentType, instructions string) json.RawMessage {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"agent_type":   agentType,
		"instructions": instructions,
		"tools": []any{
			map[string]any{"type": "slack", "toolkit_name": "chat", "name": "chat", "settings": map[string]any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

const skipsTheHITL = `
entry_point: draft
nodes:
  - id: draft
    type: llm
    transition: post
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`

const passesTheHITL = `
entry_point: draft
nodes:
  - id: draft
    type: llm
    transition: review
  - id: review
    type: hitl
    routes: {approve: post, reject: END}
  - id: post
    type: toolkit
    toolkit_name: chat
    tool: send_message
    transition: END
`

// Verification (ADR-0041 decision 5): under a policy that requires a hitl
// node, a pipeline with a path to the send tool that skips it is refused;
// the same pipeline with the hitl node starts.
func TestAdmitPipeline(t *testing.T) {
	t.Parallel()
	requiring := saved()
	requiring.Policy.RequireHITL = []agentlifecycle.Tier{agentlifecycle.TierHigh, agentlifecycle.TierCritical}
	tiers := &tierStub{tiers: map[[2]int64]string{{17, 5}: "high", {17, 6}: "low"}}
	admission, err := NewPipelineAdmission(settingsStub{settings: requiring}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = admission.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", skipsTheHITL))
	if refusal := approvalpolicy.Refusal(err); refusal == nil || refusal.Code != approvalpolicy.CodeHITLRequired {
		t.Fatalf("a high-tier pipeline that skips the hitl node is admitted: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", passesTheHITL)); err != nil {
		t.Fatalf("a pipeline with the hitl node is refused: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 6, frozen(t, "pipeline", skipsTheHITL)); err != nil {
		t.Fatalf("a low-tier pipeline is refused: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 5, frozen(t, "react", skipsTheHITL)); err != nil {
		t.Fatalf("an agent that is not a pipeline is refused: %v", err)
	}
	// The default policy requires no hitl node, and reads no registry.
	off := &tierStub{tiers: tiers.tiers}
	defaults, err := NewPipelineAdmission(settingsStub{settings: saved()}, off)
	if err != nil {
		t.Fatal(err)
	}
	if err := defaults.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", skipsTheHITL)); err != nil || off.reads != 0 {
		t.Fatalf("the default policy refused (%v) or read the registry (%d)", err, off.reads)
	}
}

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
	err = admission.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", skipsTheHITL), nil)
	if refusal := approvalpolicy.Refusal(err); refusal == nil || refusal.Code != approvalpolicy.CodeHITLRequired {
		t.Fatalf("a high-tier pipeline that skips the hitl node is admitted: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", passesTheHITL), nil); err != nil {
		t.Fatalf("a pipeline with the hitl node is refused: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 6, frozen(t, "pipeline", skipsTheHITL), nil); err != nil {
		t.Fatalf("a low-tier pipeline is refused: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 5, frozen(t, "react", skipsTheHITL), nil); err != nil {
		t.Fatalf("an agent that is not a pipeline is refused: %v", err)
	}
	// The default policy requires no hitl node, and reads no registry.
	off := &tierStub{tiers: tiers.tiers}
	defaults, err := NewPipelineAdmission(settingsStub{settings: saved()}, off)
	if err != nil {
		t.Fatal(err)
	}
	if err := defaults.AdmitPipeline(ctx, 17, 5, frozen(t, "pipeline", skipsTheHITL), nil); err != nil || off.reads != 0 {
		t.Fatalf("the default policy refused (%v) or read the registry (%d)", err, off.reads)
	}
}

// withChild answers a frozen version whose tools attach one saved pipeline
// (application 40+n, version n) under alias, next to the chat toolkit.
func withChild(t *testing.T, agentType, instructions, alias string, child int64) json.RawMessage {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"agent_type":   agentType,
		"instructions": instructions,
		"tools": []any{
			map[string]any{"type": "slack", "toolkit_name": "chat", "name": "chat", "settings": map[string]any{}},
			map[string]any{
				"type": "application", "name": alias, "toolkit_name": alias, "agent_type": "pipeline",
				"settings": map[string]any{"application_id": 40 + child, "application_version_id": child},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

const runsSender = `
entry_point: draft
nodes:
  - {id: draft, type: llm, transition: run}
  - {id: run, type: agent, tool: Sender, transition: END}
`

const reviewsThenRunsSender = `
entry_point: review
nodes:
  - {id: review, type: hitl, routes: {approve: run, reject: END}}
  - {id: run, type: agent, tool: Sender, transition: END}
`

// Review finding 2: a saved child pipeline an agent node runs, or a pipeline
// an agent attaches as a tool, is checked; a hitl node before the agent node
// covers the child; the child's own tier counts; cycles and depth refuse.
func TestAdmitPipelineChildren(t *testing.T) {
	t.Parallel()
	requiring := saved()
	requiring.Policy.RequireHITL = []agentlifecycle.Tier{agentlifecycle.TierHigh}
	ctx := context.Background()
	versions := map[int64]json.RawMessage{
		1: frozen(t, "pipeline", skipsTheHITL),
		2: frozen(t, "pipeline", passesTheHITL),
	}
	reader := func(reads *int) ChildVersions {
		return func(_ context.Context, applicationID, versionID int64) (json.RawMessage, error) {
			*reads++
			if applicationID != 40+versionID {
				return nil, errors.New("wrong identity")
			}
			if version, ok := versions[versionID]; ok {
				return version, nil
			}
			return nil, errors.New("not found")
		}
	}
	tiers := &tierStub{tiers: map[[2]int64]string{{17, 5}: "high", {17, 6}: "low", {17, 41}: "high"}}
	admission, err := NewPipelineAdmission(settingsStub{settings: requiring}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		application int64
		version     json.RawMessage
		refused     bool
	}{
		"an agent node runs a child that skips the hitl node": {5, withChild(t, "pipeline", runsSender, "Sender", 1), true},
		"a hitl node before the agent node covers the child":  {5, withChild(t, "pipeline", reviewsThenRunsSender, "Sender", 1), false},
		"a child with its own hitl node":                      {5, withChild(t, "pipeline", runsSender, "Sender", 2), false},
		"an agent attaches a pipeline that skips the hitl":    {5, withChild(t, "react", "be helpful", "Sender", 1), true},
		"a low-tier root runs a high-tier child":              {6, withChild(t, "pipeline", runsSender, "Sender", 1), true},
		"an ad hoc agent attaches a high-tier pipeline":       {0, withChild(t, "react", "be helpful", "Sender", 1), true},
	} {
		reads := 0
		err := admission.AdmitPipeline(ctx, 17, test.application, test.version, reader(&reads))
		if refused := approvalpolicy.Refusal(err) != nil; refused != test.refused || (!refused && err != nil) {
			t.Errorf("%s: err = %v, want refused %t", name, err, test.refused)
		}
		if reads != 1 {
			t.Errorf("%s: child reads = %d", name, reads)
		}
	}

	// A cycle and a composition deeper than the worker admits are refused.
	versions[3] = withChild(t, "pipeline", runsSender, "Sender", 3)
	versions[4] = withChild(t, "pipeline", runsSender, "Sender", 5)
	versions[5] = withChild(t, "pipeline", runsSender, "Sender", 6)
	versions[6] = withChild(t, "pipeline", runsSender, "Sender", 7)
	versions[7] = frozen(t, "pipeline", passesTheHITL)
	for name, root := range map[string]json.RawMessage{
		"a cycle":         withChild(t, "pipeline", reviewsThenRunsSender, "Sender", 3),
		"too many levels": withChild(t, "pipeline", reviewsThenRunsSender, "Sender", 4),
	} {
		reads := 0
		if err := admission.AdmitPipeline(ctx, 17, 5, root, reader(&reads)); approvalpolicy.Refusal(err) == nil {
			t.Errorf("%s is admitted: %v", name, err)
		}
	}
	// Three levels below the root are admitted.
	reads := 0
	if err := admission.AdmitPipeline(ctx, 17, 5, withChild(t, "pipeline", reviewsThenRunsSender, "Sender", 5), reader(&reads)); err != nil {
		t.Errorf("three levels are refused: %v", err)
	}

	// A child that cannot be read, and a missing reader, fail the start.
	reads = 0
	if err := admission.AdmitPipeline(ctx, 17, 5, withChild(t, "pipeline", runsSender, "Sender", 9), reader(&reads)); err == nil || approvalpolicy.Refusal(err) != nil {
		t.Errorf("an unreadable child: %v", err)
	}
	if err := admission.AdmitPipeline(ctx, 17, 5, withChild(t, "pipeline", runsSender, "Sender", 1), nil); err == nil || approvalpolicy.Refusal(err) != nil {
		t.Errorf("no reader: %v", err)
	}
}

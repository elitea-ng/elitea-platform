package workerruncaps

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type settingsStub struct {
	caps platformconfig.RunCaps
	err  error
}

func (s settingsStub) Get(context.Context) (platformconfig.RunCaps, error) { return s.caps, s.err }

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

type tierStub map[[2]int64]string

func (s tierStub) AgentRiskTier(_ context.Context, projectID, applicationID int64) (string, error) {
	return s[[2]int64{projectID, applicationID}], nil
}

type binderStub struct {
	bindings []Binding
	answer   func(Binding) platformconfig.TierRunCaps
}

func (b *binderStub) BindRunCaps(_ context.Context, binding Binding) (platformconfig.TierRunCaps, error) {
	b.bindings = append(b.bindings, binding)
	if b.answer != nil {
		return b.answer(binding), nil
	}
	return binding.Caps, nil
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
		ExecutionId:       "exec-1",
		Generation:        1,
		ResourceProjectId: "17",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "request"},
		},
	}
}

func savedCaps() platformconfig.RunCaps {
	return platformconfig.RunCapsFromValues(platformconfig.Values{
		"high_max_tool_calls":  float64(80),
		"high_max_model_turns": float64(40),
		"high_max_spend":       float64(3),
	})
}

// The rollout gate: unsaved, the command gets no field 69, but the run's caps
// (the ADR default, 200 tool calls) are still recorded for the evidence and
// the spend routine.
func TestResolverSendsNothingUntilSaved(t *testing.T) {
	t.Parallel()
	binder := &binderStub{}
	resolver, err := NewResolver(settingsStub{caps: platformconfig.RunCapsFromValues(nil)},
		inputStub{"request": agentInput(t, `{"id":5}`)}, tierStub{{17, 5}: "high"}, binder)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := resolver.WorkerRunCaps(context.Background(), agentCommand())
	if err != nil || caps != nil {
		t.Fatalf("caps = %v, err = %v", caps, err)
	}
	// The command carries no caps, so the binder must not consume a grant on
	// a worker cap.
	if len(binder.bindings) != 1 || binder.bindings[0].Caps != platformconfig.DefaultTierRunCaps() ||
		binder.bindings[0].WorkerCapsSent {
		t.Fatalf("bindings = %+v", binder.bindings)
	}
}

// Saved: the root agent's tier caps, lowered by its author's caps.
func TestResolverLowersTheTierCapsByTheAuthorCaps(t *testing.T) {
	t.Parallel()
	binder := &binderStub{}
	application := `{"id":5,"version_details":{"meta":{"run_caps":{"max_tool_calls":20,"max_model_turns":90,"max_spend":1.5}}}}`
	resolver, err := NewResolver(settingsStub{caps: savedCaps()},
		inputStub{"request": agentInput(t, application)}, tierStub{{17, 5}: "high"}, binder)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := resolver.WorkerRunCaps(context.Background(), agentCommand())
	if err != nil {
		t.Fatal(err)
	}
	want := &runtimev1.WorkerRunCapsV1{MaxToolCalls: 20, MaxModelTurns: 40}
	if !proto.Equal(caps, want) {
		t.Fatalf("caps = %v, want %v", caps, want)
	}
	if got := binder.bindings[0]; got.ExecutionID != "exec-1" || got.ProjectID != 17 || !got.WorkerCapsSent ||
		got.Caps != (platformconfig.TierRunCaps{MaxToolCalls: 20, MaxModelTurns: 40, MaxSpend: 1.5}) {
		t.Fatalf("binding = %+v", got)
	}
}

// A consumed grant comes back from the binder and reaches the command.
func TestResolverSendsTheBoundCaps(t *testing.T) {
	t.Parallel()
	binder := &binderStub{answer: func(b Binding) platformconfig.TierRunCaps {
		return b.Caps.Raised(platformconfig.RunCapMaxToolCalls)
	}}
	resolver, err := NewResolver(settingsStub{caps: savedCaps()},
		inputStub{"request": agentInput(t, `{"id":5}`)}, tierStub{{17, 5}: "high"}, binder)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := resolver.WorkerRunCaps(context.Background(), agentCommand())
	if err != nil || caps.GetMaxToolCalls() != 160 {
		t.Fatalf("caps = %v, err = %v", caps, err)
	}
}

func TestResolverSkipsOtherCommandsAndFailsClosed(t *testing.T) {
	t.Parallel()
	resolver, err := NewResolver(settingsStub{err: errors.New("down")}, inputStub{}, tierStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if caps, err := resolver.WorkerRunCaps(context.Background(), &runtimev1.WorkerCommandV1{}); caps != nil || err != nil {
		t.Fatalf("a non-agent command got caps %v, %v", caps, err)
	}
	if _, err := resolver.WorkerRunCaps(context.Background(), agentCommand()); err == nil {
		t.Fatal("an unreadable section signed a command")
	}
}

func TestAuthorCaps(t *testing.T) {
	t.Parallel()
	caps, invalid := AuthorCaps(map[string]any{
		"max_tool_calls": float64(10), "max_records_read": float64(500), "max_spend": 0.25,
		"max_model_turns": nil, "max_wall_seconds": float64(60), "bogus": "x",
	})
	if caps != (platformconfig.TierRunCaps{MaxToolCalls: 10, MaxRecordsRead: 500, MaxSpend: 0.25}) {
		t.Fatalf("caps = %+v", caps)
	}
	if len(invalid) != 2 {
		t.Fatalf("invalid = %v", invalid)
	}
	if _, invalid := AuthorCaps("nope"); len(invalid) != 1 {
		t.Fatal("a non-object was accepted")
	}
	if caps, invalid := AuthorCaps(nil); caps != (platformconfig.TierRunCaps{}) || invalid != nil {
		t.Fatal("null was not none")
	}
}

func TestCheckerRefusesARaise(t *testing.T) {
	t.Parallel()
	checker, err := NewChecker(settingsStub{caps: savedCaps()}, tierStub{{17, 5}: "high"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := checker.Check(ctx, 17, 5, map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(80)}}); err != nil {
		t.Fatalf("an equal cap was refused: %v", err)
	}
	err = checker.Check(ctx, 17, 5, map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(81)}})
	var refusal *CapRaiseError
	if !errors.As(err, &refusal) || refusal.Code != CodeCapAboveTier || refusal.Cap != "max_tool_calls" ||
		refusal.Tier != agentlifecycle.TierHigh || refusal.TierCap != 80 || refusal.Requested != 81 {
		t.Fatalf("refusal = %+v (%v)", refusal, err)
	}
	// No tier cap (records read off) accepts any author value.
	if err := checker.Check(ctx, 17, 5, map[string]any{"run_caps": map[string]any{"max_records_read": float64(1e6)}}); err != nil {
		t.Fatalf("lowering an off cap was refused: %v", err)
	}
	// A new agent is low tier: the default 200 tool calls.
	if err := checker.Check(ctx, 17, 0, map[string]any{"run_caps": map[string]any{"max_tool_calls": float64(201)}}); err == nil {
		t.Fatal("a new agent raised the default")
	}
	err = checker.Check(ctx, 17, 5, map[string]any{"run_caps": map[string]any{"max_tool_calls": 1.5}})
	if !errors.As(err, &refusal) || refusal.Code != CodeCapInvalid {
		t.Fatalf("an invalid cap = %v", err)
	}
	if err := checker.Check(ctx, 17, 5, map[string]any{"step_limit": float64(5)}); err != nil {
		t.Fatalf("meta without run_caps = %v", err)
	}
}

type storeStub struct{ requests []OverrideRequest }

func (s *storeStub) GrantRunCapOverride(_ context.Context, request OverrideRequest) (OverrideGrant, error) {
	s.requests = append(s.requests, request)
	return OverrideGrant{ID: 1, Cap: "max_tool_calls"}, nil
}

func TestOverrideServiceValidatesAndStampsTheGate(t *testing.T) {
	t.Parallel()
	store := &storeStub{}
	service, err := NewOverrideService(store, settingsStub{caps: savedCaps()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	message := "0190f2a6-7d1c-7cc1-9a35-3f4b4d2a1b11"
	for _, bad := range []OverrideRequest{
		{ProjectID: 0, ActorUserID: 1, MessageID: message},
		{ProjectID: 1, ActorUserID: 0, MessageID: message},
		{ProjectID: 1, ActorUserID: 1, MessageID: "x"},
		{ProjectID: 1, ActorUserID: 1, MessageID: message, ConversationUUID: "nope"},
	} {
		if _, err := service.Grant(ctx, bad); !errors.Is(err, ErrOverrideInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := service.Grant(ctx, OverrideRequest{ProjectID: 1, ActorUserID: 2, MessageID: message}); err != nil {
		t.Fatal(err)
	}
	if got := store.requests[0]; !got.AllowWorkerCaps || got.ExpiresIn != time.Hour {
		t.Fatalf("request = %+v", got)
	}
}

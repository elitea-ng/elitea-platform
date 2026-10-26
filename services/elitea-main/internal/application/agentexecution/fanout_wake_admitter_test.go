package agentexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

const wakeTestGeneration = "10000000-0000-4000-8000-0000000000a1"

func wakeTestTarget(t *testing.T, mutate func(*runtimev1.AgentExecutionInputV1)) FanoutWakeTarget {
	t.Helper()
	input := validAgentInput()
	input.ExecutionGeneration = proto.String(wakeTestGeneration)
	input.Meta = []byte(`{"keep":1,"pipeline_static_tool_resume_v1":{"x":1}}`)
	input.HitlResume, input.ShouldContinue = true, true
	input.HitlAction = proto.String("approve")
	input.HitlDecisions = []byte(`[{"interrupt_id":"x","action":"approve"}]`)
	if mutate != nil {
		mutate(input)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return FanoutWakeTarget{
		Turn: CurrentFanoutWakeTurn{ProjectID: 1, ActorUserID: 8, ConversationUUID: "10000000-0000-4000-8000-000000000001",
			ResponseMessageID: "10000000-0000-4000-8000-000000000002", ExecutionGeneration: wakeTestGeneration,
			ThreadID: "thread-1", DecisionRevision: 3, ParkedExecutionID: strings.Repeat("a", 32), ParkedGeneration: 1},
		TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "8",
		CapabilityID: executiondomain.AgentApplicationCapability, FrozenInput: encoded, FrozenInputDigest: digest[:],
	}
}

func TestFanoutWakeInputKeepsFrozenFieldsAndCarriesOnlyTheMarker(t *testing.T) {
	target := wakeTestTarget(t, nil)
	input, err := FanoutWakeInput(target)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fanout_wake_v1":{"decision_revision":3,"response_message_id":"10000000-0000-4000-8000-000000000002","schema":"elitea.pipeline.fanout-wake.v1"},"keep":1}`
	if string(input.GetMeta()) != want || input.GetHitlResume() || input.GetShouldContinue() || input.HitlAction != nil ||
		string(input.GetHitlDecisions()) != `[]` || string(input.GetUserInput()) != `""` || input.GetThreadId() != "thread-1" ||
		string(input.GetLlm()) != `{"kwargs":{"model":"model-1"}}` || validateAuthoritativeInput(input) != nil {
		t.Fatalf("wake input = %v", input)
	}
	again, err := FanoutWakeInput(target)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	second, _ := proto.MarshalOptions{Deterministic: true}.Marshal(again)
	if !bytes.Equal(first, second) {
		t.Fatal("the wake input is not deterministic, so a retried admission would conflict")
	}

	refused := map[string]FanoutWakeTarget{}
	digest := wakeTestTarget(t, nil)
	digest.FrozenInputDigest = make([]byte, sha256.Size)
	refused["digest"] = digest
	refused["generation"] = wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ExecutionGeneration = proto.String("other") })
	refused["thread"] = wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ThreadId = proto.String("other") })
	refused["conversation"] = wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ConversationId = proto.String("other") })
	refused["meta"] = wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.Meta = []byte(`[]`) })
	turn := wakeTestTarget(t, nil)
	turn.Turn.DecisionRevision = 0
	refused["turn"] = turn
	for name, target := range refused {
		if _, err := FanoutWakeInput(target); !errors.Is(err, errInvalidFanoutWake) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

type fakeWakeStore struct {
	mu      sync.Mutex
	targets []FanoutWakeTarget
	failed  []string
	claims  int
}

func (s *fakeWakeStore) ClaimFanoutWakes(context.Context, int, time.Duration, int) ([]FanoutWakeTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	targets := s.targets
	s.targets = nil
	return targets, nil
}

func (s *fakeWakeStore) FailFanoutWake(_ context.Context, turn CurrentFanoutWakeTurn, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, code)
	return nil
}

type fakeWakeSubmitter struct {
	err      error
	requests []SubmitRequest
}

func (s *fakeWakeSubmitter) Submit(_ context.Context, request SubmitRequest) (executionapp.AdmissionOutcome, error) {
	s.requests = append(s.requests, request)
	if s.err != nil {
		return executionapp.AdmissionOutcome{}, s.err
	}
	return executionapp.AdmissionOutcome{ExecutionID: "e", Created: true}, nil
}

func TestFanoutWakeAdmitterMapsRefusalsToTypedFailures(t *testing.T) {
	cases := map[error]string{
		ErrFanoutWakeActorNotAllowed:               FanoutWakeFailureActorNotAllowed,
		ErrFanoutWakeTargetGone:                    FanoutWakeFailureResponseNotParked,
		executionapp.ErrIdempotencyConflict:        FanoutWakeFailureAdmission,
		ErrFanoutWakeNotQueued:                     "",
		executionapp.ErrAdmissionCapacityExhausted: "",
		errors.New("database unavailable"):         "",
	}
	for submitErr, wantCode := range cases {
		var logs bytes.Buffer
		store := &fakeWakeStore{targets: []FanoutWakeTarget{wakeTestTarget(t, nil)}}
		submitter := &fakeWakeSubmitter{err: submitErr}
		admitter, err := NewFanoutWakeAdmitter(store, submitter, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		created, err := admitter.Sweep(t.Context())
		if err != nil || created != 0 {
			t.Fatalf("%v: created=%d err=%v", submitErr, created, err)
		}
		if (wantCode == "" && len(store.failed) != 0) || (wantCode != "" && (len(store.failed) != 1 || store.failed[0] != wantCode)) {
			t.Errorf("%v: failed=%v want %q", submitErr, store.failed, wantCode)
		}
		if strings.Contains(logs.String(), "10000000-0000-4000-8000-000000000002") || strings.Contains(logs.String(), "thread-1") {
			t.Errorf("%v: log carries identities: %s", submitErr, logs.String())
		}
	}
	// A corrupt frozen input never reaches admission.
	store := &fakeWakeStore{targets: []FanoutWakeTarget{wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) {
		input.ExecutionGeneration = proto.String("other")
	})}}
	submitter := &fakeWakeSubmitter{}
	admitter, _ := NewFanoutWakeAdmitter(store, submitter, slog.New(slog.DiscardHandler))
	if _, err := admitter.Sweep(t.Context()); err != nil || len(submitter.requests) != 0 ||
		len(store.failed) != 1 || store.failed[0] != FanoutWakeFailureParkedInput {
		t.Fatalf("corrupt input: %v %d %v", err, len(submitter.requests), store.failed)
	}
	// An admitted wake is submitted as the contract's continuation.
	store = &fakeWakeStore{targets: []FanoutWakeTarget{wakeTestTarget(t, nil)}}
	submitter = &fakeWakeSubmitter{}
	admitter, _ = NewFanoutWakeAdmitter(store, submitter, slog.New(slog.DiscardHandler))
	if created, err := admitter.Sweep(t.Context()); err != nil || created != 1 {
		t.Fatalf("admit: %d %v", created, err)
	}
	request := submitter.requests[0]
	if request.IdempotencyKey != "continue-interrupt/10000000-0000-4000-8000-000000000002/3" ||
		request.SIOEvent != "chat_continue_predict" || request.CurrentFanoutWakeTurn == nil ||
		request.CurrentContinueTurn != nil || request.Identity.ActorID != "8" {
		t.Fatalf("submit request = %+v", request)
	}
}

func TestFanoutWakeAdmitterKickIsNonBlockingAndRunStops(t *testing.T) {
	store := &fakeWakeStore{}
	admitter, err := NewFanoutWakeAdmitter(store, &fakeWakeSubmitter{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	admitter.interval = time.Hour
	for range 100 {
		admitter.Kick() // coalesces; never blocks without a runner
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- admitter.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		claims := store.claims
		store.mu.Unlock()
		if claims >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a pending kick did not run a pass")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 1 {
		t.Fatalf("100 coalesced kicks ran %d passes", store.claims)
	}
	var nilAdmitter *FanoutWakeAdmitter
	nilAdmitter.Kick()
}

func TestSubmitRefusesAMalformedWakeTurn(t *testing.T) {
	store := &recordingAdmissionStore{}
	counter := 0
	nextID := func() (string, error) { counter++; return "id-" + strconv.Itoa(counter), nil }
	factory, err := NewInputBundleFactory(InputProfile{Classification: "tenant-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1"}, nextID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAdmissionService(store, factory, nil, nextID)
	if err != nil {
		t.Fatal(err)
	}
	target := wakeTestTarget(t, nil)
	input, err := FanoutWakeInput(target)
	if err != nil {
		t.Fatal(err)
	}
	base := SubmitRequest{
		Identity:       executionapp.AdmissionIdentity{TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "8"},
		IdempotencyKey: FanoutWakeIdempotencyKey(target.Turn.ResponseMessageID, 3), CapabilityID: target.CapabilityID,
		ClientStreamID: target.Turn.ConversationUUID, ClientMessageID: target.Turn.ResponseMessageID,
		SIOEvent: "chat_continue_predict", Input: input, CurrentFanoutWakeTurn: target.Turn.Clone(),
	}
	for name, mutate := range map[string]func(*SubmitRequest){
		"wrong key":      func(r *SubmitRequest) { r.IdempotencyKey = "continue/other" },
		"wrong event":    func(r *SubmitRequest) { r.SIOEvent = "chat_predict" },
		"other actor":    func(r *SubmitRequest) { r.Identity.ActorID = "9" },
		"other response": func(r *SubmitRequest) { r.ClientMessageID = "10000000-0000-4000-8000-000000000009" },
		"two turns":      func(r *SubmitRequest) { r.CurrentContinueTurn = &CurrentContinueTurn{} },
		"invalid wake":   func(r *SubmitRequest) { r.CurrentFanoutWakeTurn.ParkedExecutionID = "nope" },
	} {
		request := base
		request.CurrentFanoutWakeTurn = base.CurrentFanoutWakeTurn.Clone()
		mutate(&request)
		if _, err := service.Submit(t.Context(), request); !errors.Is(err, ErrInvalidAgentAdmission) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if store.admission.Record.Job.ID != "" {
		t.Fatal("a refused wake reached storage")
	}
}

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
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/idlecompaction"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

var idleTestStarted = time.UnixMilli(1_770_000_000_123).UTC()

func idleTestTarget(t *testing.T, mutate func(*runtimev1.AgentExecutionInputV1)) IdleCompactionTarget {
	t.Helper()
	input := validAgentInput()
	input.ExecutionGeneration = proto.String(wakeTestGeneration)
	input.Meta = []byte(`{"keep":1,"fanout_wake_v1":{"x":1},"pipeline_static_resume_v1":{"y":2}}`)
	input.HitlResume, input.ShouldContinue, input.IsRegenerate = true, true, true
	input.HitlAction = proto.String("approve")
	input.HitlValue = proto.String("yes")
	input.HitlDecisions = []byte(`[{"interrupt_id":"x","action":"approve"}]`)
	input.CheckpointId = proto.String("checkpoint-1")
	input.TruncatedContent = []byte(`"cut"`)
	if mutate != nil {
		mutate(input)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return IdleCompactionTarget{
		Turn: CurrentIdleCompactionTurn{ProjectID: 1, ActorUserID: 8, ConversationUUID: "10000000-0000-4000-8000-000000000001",
			ResponseMessageID: "10000000-0000-4000-8000-000000000002", ParticipantID: 5,
			SourceExecutionID: strings.Repeat("b", 32), SourceGeneration: 1, ExecutionGeneration: wakeTestGeneration,
			ThreadID: "thread-1", ApplicationID: 7},
		TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "8",
		CapabilityID: executiondomain.AgentApplicationCapability, FrozenInput: encoded, FrozenInputDigest: digest[:],
		RequestStarted: idleTestStarted, CacheTTL: 5 * time.Minute,
		LatestExecution: strings.Repeat("b", 32), Now: idleTestStarted.Add(4 * time.Minute),
	}
}

func TestIdleCompactionMetaIsTheExactCanonicalMarker(t *testing.T) {
	meta, err := IdleCompactionMeta(strings.Repeat("b", 32), 2, idleTestStarted, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cache_ttl_seconds":300,"request_started_unix_ms":1770000000123,"schema":"elitea.agent.idle-compaction.v1","source_execution_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","source_generation":2}`
	if string(meta) != want {
		t.Fatalf("meta = %s", meta)
	}
	for name, call := range map[string]func() error{
		"id": func() error { _, err := IdleCompactionMeta("nope", 1, idleTestStarted, 5*time.Minute); return err },
		"generation": func() error {
			_, err := IdleCompactionMeta(strings.Repeat("b", 32), 0, idleTestStarted, 5*time.Minute)
			return err
		},
		"started": func() error {
			_, err := IdleCompactionMeta(strings.Repeat("b", 32), 1, time.Time{}, 5*time.Minute)
			return err
		},
		"ttl": func() error {
			_, err := IdleCompactionMeta(strings.Repeat("b", 32), 1, idleTestStarted, time.Second)
			return err
		},
	} {
		if !errors.Is(call(), errInvalidIdleCompaction) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestIdleCompactionInputKeepsFrozenFieldsAndClearsTheTurn(t *testing.T) {
	target := idleTestTarget(t, nil)
	input, err := IdleCompactionInput(target)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"idle_compaction_v1":{"cache_ttl_seconds":300,"request_started_unix_ms":1770000000123,"schema":"elitea.agent.idle-compaction.v1","source_execution_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","source_generation":1}}`
	if string(input.GetMeta()) != want || input.GetHitlResume() || input.GetShouldContinue() || input.GetIsRegenerate() ||
		input.HitlAction != nil || input.HitlValue != nil || input.CheckpointId != nil || input.TruncatedContent != nil ||
		input.McpTokenReferences != nil || string(input.GetHitlDecisions()) != `[]` || string(input.GetUserInput()) != `""` ||
		input.GetThreadId() != "thread-1" || string(input.GetLlm()) != `{"kwargs":{"model":"model-1"}}` ||
		string(input.GetApplication()) != `{"id":7,"version_id":9}` || validateAuthoritativeInput(input) != nil {
		t.Fatalf("idle input = %v", input)
	}
	again, err := IdleCompactionInput(target)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	second, _ := proto.MarshalOptions{Deterministic: true}.Marshal(again)
	if !bytes.Equal(first, second) {
		t.Fatal("the idle input is not deterministic, so a retried admission would conflict")
	}

	refused := map[string]IdleCompactionTarget{}
	digest := idleTestTarget(t, nil)
	digest.FrozenInputDigest = make([]byte, sha256.Size)
	refused["digest"] = digest
	refused["generation"] = idleTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ExecutionGeneration = proto.String("other") })
	refused["thread"] = idleTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ThreadId = proto.String("other") })
	refused["conversation"] = idleTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.ConversationId = proto.String("other") })
	refused["meta"] = idleTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) { input.Meta = []byte(`[]`) })
	started := idleTestTarget(t, nil)
	started.RequestStarted = time.Time{}
	refused["started"] = started
	turn := idleTestTarget(t, nil)
	turn.Turn.ParticipantID = 0
	refused["turn"] = turn
	for name, target := range refused {
		if _, err := IdleCompactionInput(target); !errors.Is(err, errInvalidIdleCompaction) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIdleCompactionPromptCacheIsNativeAnthropicOnly(t *testing.T) {
	cases := map[string]struct {
		llm, application string
		want             bool
	}{
		"agent claude":      {`{"kwargs":{"openai_compatible":false}}`, `{"version_details":{"llm_settings":{"model_name":"claude-sonnet-4-5"}}}`, true},
		"adhoc anthropic":   {`{"kwargs":{"model":"anthropic.claude-x","openai_compatible":false}}`, `{}`, true},
		"openai compatible": {`{"kwargs":{"model":"claude-x","openai_compatible":true}}`, `{}`, false},
		"compat unknown":    {`{"kwargs":{"model":"claude-x"}}`, `{}`, false},
		"other model":       {`{"kwargs":{"model":"gpt-5","openai_compatible":false}}`, `{}`, false},
		"settings fallback": {`{}`, `{"version_details":{"llm_settings":{"model_name":"Claude","openai_compatible":false}}}`, true},
		"garbage is no":     {`[`, `[`, false},
	}
	for name, c := range cases {
		input := validAgentInput()
		input.Llm, input.Application = []byte(c.llm), []byte(c.application)
		if got := IdleCompactionPromptCache(input); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if IdleCompactionPromptCache(nil) {
		t.Fatal("nil input has a cache")
	}
}

type fakeIdleStore struct {
	mu       sync.Mutex
	targets  []IdleCompactionTarget
	skipped  []string
	deferred int
	claims   int
}

func (s *fakeIdleStore) ClaimIdleCompactions(context.Context, int, time.Duration, int) ([]IdleCompactionTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	targets := s.targets
	s.targets = nil
	return targets, nil
}

func (s *fakeIdleStore) SkipIdleCompaction(_ context.Context, _ CurrentIdleCompactionTurn, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, code)
	return nil
}

func (s *fakeIdleStore) DeferIdleCompaction(context.Context, CurrentIdleCompactionTurn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferred++
	return nil
}

func TestIdleCompactionAdmitterClassifiesRefusals(t *testing.T) {
	cases := map[error]string{
		ErrIdleCompactionSuperseded:                string(idlecompaction.SkipSuperseded),
		ErrIdleCompactionAgentBusy:                 "",
		ErrIdleCompactionActorNotAllowed:           IdleCompactionSkipActorNotAllowed,
		ErrUnsupportedCurrentAgentStart:            IdleCompactionSkipAdmission,
		executionapp.ErrIdempotencyConflict:        IdleCompactionSkipAdmission,
		ErrProjectSkillUnknown:                     IdleCompactionSkipAdmission,
		ErrIdleCompactionNotQueued:                 "",
		executionapp.ErrAdmissionCapacityExhausted: "",
		errors.New("database unavailable"):         "",
	}
	for submitErr, wantCode := range cases {
		var logs bytes.Buffer
		store := &fakeIdleStore{targets: []IdleCompactionTarget{idleTestTarget(t, nil)}}
		submitter := &fakeWakeSubmitter{err: submitErr}
		admitter, err := NewIdleCompactionAdmitter(store, submitter, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if created, err := admitter.Sweep(t.Context()); err != nil || created != 0 {
			t.Fatalf("%v: created=%d err=%v", submitErr, created, err)
		}
		if (wantCode == "" && len(store.skipped) != 0) || (wantCode != "" && (len(store.skipped) != 1 || store.skipped[0] != wantCode)) {
			t.Errorf("%v: skipped=%v want %q", submitErr, store.skipped, wantCode)
		}
		waits := errors.Is(submitErr, executionapp.ErrAdmissionCapacityExhausted) || errors.Is(submitErr, ErrIdleCompactionAgentBusy)
		if (store.deferred == 1) != waits {
			t.Errorf("%v: deferred=%d", submitErr, store.deferred)
		}
		if strings.Contains(logs.String(), "10000000-0000-4000-8000-000000000002") || strings.Contains(logs.String(), "thread-1") {
			t.Errorf("%v: log carries identities: %s", submitErr, logs.String())
		}
	}

	// The decision runs before admission: nothing is submitted for a check
	// that is superseded or past its cache. A live run of the conversation
	// (the agent's own or a peer's, which shares its session) defers it.
	{
		target := idleTestTarget(t, nil)
		target.AgentBusy = true
		store := &fakeIdleStore{targets: []IdleCompactionTarget{target}}
		submitter := &fakeWakeSubmitter{}
		admitter, _ := NewIdleCompactionAdmitter(store, submitter, slog.New(slog.DiscardHandler))
		if _, err := admitter.Sweep(t.Context()); err != nil || len(submitter.requests) != 0 ||
			len(store.skipped) != 0 || store.deferred != 1 {
			t.Errorf("busy: %v %d %v %d", err, len(submitter.requests), store.skipped, store.deferred)
		}
	}
	decided := map[string]func(*IdleCompactionTarget){
		string(idlecompaction.SkipSuperseded): func(target *IdleCompactionTarget) { target.LatestExecution = strings.Repeat("c", 32) },
		string(idlecompaction.SkipCacheExpired): func(target *IdleCompactionTarget) {
			target.Now = target.RequestStarted.Add(10 * time.Minute)
		},
	}
	for code, mutate := range decided {
		target := idleTestTarget(t, nil)
		mutate(&target)
		store := &fakeIdleStore{targets: []IdleCompactionTarget{target}}
		submitter := &fakeWakeSubmitter{}
		admitter, _ := NewIdleCompactionAdmitter(store, submitter, slog.New(slog.DiscardHandler))
		if _, err := admitter.Sweep(t.Context()); err != nil || len(submitter.requests) != 0 ||
			len(store.skipped) != 1 || store.skipped[0] != code {
			t.Errorf("%s: %v %d %v", code, err, len(submitter.requests), store.skipped)
		}
	}

	// A corrupt frozen input never reaches admission.
	store := &fakeIdleStore{targets: []IdleCompactionTarget{idleTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) {
		input.ExecutionGeneration = proto.String("other")
	})}}
	submitter := &fakeWakeSubmitter{}
	admitter, _ := NewIdleCompactionAdmitter(store, submitter, slog.New(slog.DiscardHandler))
	if _, err := admitter.Sweep(t.Context()); err != nil || len(submitter.requests) != 0 ||
		len(store.skipped) != 1 || store.skipped[0] != IdleCompactionSkipSourceInvalid {
		t.Fatalf("corrupt input: %v %d %v", err, len(submitter.requests), store.skipped)
	}

	// A due check is submitted as the idle continuation of its source.
	store = &fakeIdleStore{targets: []IdleCompactionTarget{idleTestTarget(t, nil)}}
	submitter = &fakeWakeSubmitter{}
	admitter, _ = NewIdleCompactionAdmitter(store, submitter, slog.New(slog.DiscardHandler))
	if created, err := admitter.Sweep(t.Context()); err != nil || created != 1 {
		t.Fatalf("admit: %d %v", created, err)
	}
	request := submitter.requests[0]
	if request.IdempotencyKey != "idle-compaction/"+strings.Repeat("b", 32) || request.SIOEvent != "chat_continue_predict" ||
		request.ClientStreamID != "10000000-0000-4000-8000-000000000001" ||
		request.ClientMessageID != "10000000-0000-4000-8000-000000000002" ||
		request.CurrentIdleCompactionTurn == nil || request.CurrentContinueTurn != nil || request.Identity.ActorID != "8" ||
		!strings.Contains(string(request.Input.GetMeta()), `"idle_compaction_v1"`) {
		t.Fatalf("submit request = %+v", request)
	}
}

func TestSubmitRefusesAMalformedIdleCompactionTurn(t *testing.T) {
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
	target := idleTestTarget(t, nil)
	input, err := IdleCompactionInput(target)
	if err != nil {
		t.Fatal(err)
	}
	base := SubmitRequest{
		Identity:       executionapp.AdmissionIdentity{TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "8"},
		IdempotencyKey: IdleCompactionIdempotencyKey(target.Turn.SourceExecutionID), CapabilityID: target.CapabilityID,
		ClientStreamID: target.Turn.ConversationUUID, ClientMessageID: target.Turn.ResponseMessageID,
		SIOEvent: "chat_continue_predict", Input: input, CurrentIdleCompactionTurn: target.Turn.Clone(),
	}
	for name, mutate := range map[string]func(*SubmitRequest){
		"wrong key":        func(r *SubmitRequest) { r.IdempotencyKey = "idle-compaction/other" },
		"wrong event":      func(r *SubmitRequest) { r.SIOEvent = "chat_predict" },
		"other actor":      func(r *SubmitRequest) { r.Identity.ActorID = "9" },
		"other response":   func(r *SubmitRequest) { r.ClientMessageID = "10000000-0000-4000-8000-000000000009" },
		"other stream":     func(r *SubmitRequest) { r.ClientStreamID = "10000000-0000-4000-8000-000000000009" },
		"with a wake":      func(r *SubmitRequest) { r.CurrentFanoutWakeTurn = &CurrentFanoutWakeTurn{} },
		"with a continue":  func(r *SubmitRequest) { r.CurrentContinueTurn = &CurrentContinueTurn{} },
		"adhoc capability": func(r *SubmitRequest) { r.CapabilityID = executiondomain.AgentAdhocCapability },
		"invalid turn":     func(r *SubmitRequest) { r.CurrentIdleCompactionTurn.SourceExecutionID = "nope" },
	} {
		request := base
		request.CurrentIdleCompactionTurn = base.CurrentIdleCompactionTurn.Clone()
		mutate(&request)
		if _, err := service.Submit(t.Context(), request); !errors.Is(err, ErrInvalidAgentAdmission) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if store.admission.Record.Job.ID != "" {
		t.Fatal("a refused idle compaction reached storage")
	}
	// A valid turn reaches storage (the recording store answers no outcome).
	if _, err := service.Submit(t.Context(), base); errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("a valid idle compaction was refused: %v", err)
	}
	if store.admission.CurrentIdleCompactionTurn == nil || store.admission.ParticipantID() != 0 ||
		store.admission.ApplicationID() != 7 {
		t.Fatalf("admission = %+v", store.admission.CurrentIdleCompactionTurn)
	}
}

type fakeIdleArmStore struct {
	armed    bool
	armCalls int
	settled  []executionapp.SettlementOutcome
	turn     idlecompaction.Turn
}

func (s *fakeIdleArmStore) ArmIdleCompaction(_ context.Context, _ string, _ uint64,
	arm func(idlecompaction.Turn) (time.Time, idlecompaction.SkipCode)) (bool, error) {
	s.armCalls++
	_, skip := arm(s.turn)
	return s.armed && skip == idlecompaction.SkipNone, nil
}

func (s *fakeIdleArmStore) SettleIdleCompactionRun(_ context.Context, _ string, outcome executionapp.SettlementOutcome) error {
	s.settled = append(s.settled, outcome)
	return nil
}

func TestIdleCompactionSettleHookArmsOnlyASucceededAgentExecution(t *testing.T) {
	id := strings.Repeat("d", 32)
	worth := idlecompaction.Turn{Succeeded: true, PromptCache: true, MessageTokens: idlecompaction.MinMessageTokens,
		RequestStarted: idleTestStarted}
	cases := []struct {
		name     string
		output   string
		outcome  executionapp.SettlementOutcome
		turn     idlecompaction.Turn
		armCalls int
		kicked   bool
	}{
		{"answered", "agent-execution:" + id, executionapp.SettlementSucceeded, worth, 1, true},
		{"too small", "agent-execution:" + id, executionapp.SettlementSucceeded, idlecompaction.Turn{Succeeded: true, PromptCache: true, MessageTokens: 10, RequestStarted: idleTestStarted}, 1, false},
		{"failed", "agent-execution:" + id, executionapp.SettlementFailed, worth, 0, false},
		{"not an agent", "configuration-validation:" + id, executionapp.SettlementSucceeded, worth, 0, false},
	}
	for _, c := range cases {
		store := &fakeIdleArmStore{armed: true, turn: c.turn}
		admitter, _ := NewIdleCompactionAdmitter(&fakeIdleStore{}, &fakeWakeSubmitter{}, slog.New(slog.DiscardHandler))
		hook, err := NewIdleCompactionSettleHook(store, admitter, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		hook.AfterSettle(t.Context(), executionapp.SettlementProposal{
			Fence: runtimedomain.Fence{ExecutionID: id, Generation: 1}, TerminalLogicalOutputID: c.output,
		}, executionapp.SettlementReceipt{Outcome: c.outcome})
		kicked := len(admitter.kick) == 1
		agent := strings.HasPrefix(c.output, "agent-execution:")
		if store.armCalls != c.armCalls || kicked != c.kicked || (len(store.settled) == 1) != agent {
			t.Errorf("%s: arm=%d kicked=%v settled=%v", c.name, store.armCalls, kicked, store.settled)
		}
	}
}

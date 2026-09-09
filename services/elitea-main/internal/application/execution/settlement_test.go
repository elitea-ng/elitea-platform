package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type memorySettlementRepository struct {
	ready     bool
	proposals map[string]SettlementProposal
	receipts  map[string]SettlementReceipt
}

func newMemorySettlementRepository() *memorySettlementRepository {
	return &memorySettlementRepository{
		ready:     true,
		proposals: make(map[string]SettlementProposal),
		receipts:  make(map[string]SettlementReceipt),
	}
}

func (r *memorySettlementRepository) PrepareSettlement(_ context.Context, proposal SettlementProposal) (SettlementReceipt, error) {
	if !r.ready {
		return SettlementReceipt{}, ErrTerminalOutputNotReady
	}
	key := fmt.Sprintf("%s:%d:%s", proposal.Fence.ExecutionID, proposal.Fence.Generation, proposal.IdempotencyKey)
	if existing, ok := r.proposals[key]; ok {
		if existing.ProposalDigest != proposal.ProposalDigest {
			return SettlementReceipt{}, ErrSettlementConflict
		}
		return r.receipts[key], nil
	}
	receipt := SettlementReceipt{ID: "receipt-1", Outcome: proposal.Outcome}
	r.proposals[key] = proposal
	r.receipts[key] = receipt
	return receipt, nil
}

func TestSettlementServiceIsGenerationBoundAndDigestIdempotent(t *testing.T) {
	repository := newMemorySettlementRepository()
	service, err := NewSettlementService(repository)
	if err != nil {
		t.Fatal(err)
	}
	proposal := validSettlementProposal()

	first, err := service.PrepareSettlement(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.PrepareSettlement(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if replay != first || len(repository.proposals) != 1 {
		t.Fatalf("identical settlement was not replayed: first=%+v replay=%+v", first, replay)
	}

	conflict := proposal
	conflict.ProposalDigest = runtimedomain.SHA256([]byte("different-proposal"))
	if _, err := service.PrepareSettlement(context.Background(), conflict); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("reused idempotency key with different digest was accepted: %v", err)
	}

	nextGeneration := proposal
	nextGeneration.Fence.Generation++
	nextGeneration.ProposalDigest = runtimedomain.SHA256([]byte("next-generation"))
	if _, err := service.PrepareSettlement(context.Background(), nextGeneration); err != nil {
		t.Fatalf("next execution generation was not independently keyed: %v", err)
	}
	if len(repository.proposals) != 2 {
		t.Fatalf("settlement repository was not keyed by generation: %d", len(repository.proposals))
	}
}

func TestSettlementServiceDoesNotPrepareBeforeTerminalOutputIsDurable(t *testing.T) {
	repository := newMemorySettlementRepository()
	repository.ready = false
	service, err := NewSettlementService(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareSettlement(context.Background(), validSettlementProposal()); !errors.Is(err, ErrTerminalOutputNotReady) {
		t.Fatalf("settlement was prepared before durable terminal output: %v", err)
	}
}

func validSettlementProposal() SettlementProposal {
	return SettlementProposal{
		Fence: runtimedomain.Fence{
			CommandID:         "command-1",
			ExecutionID:       "execution-1",
			Generation:        1,
			WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
			WorkloadSessionID: "workload-1",
			ProducerID:        "producer-1",
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("fence-token"))),
		},
		ProposalID:              "proposal-1",
		Outcome:                 SettlementSucceeded,
		TerminalLogicalOutputID: "validation:revision-1",
		TerminalEventID:         "event-1",
		TerminalSequence:        1,
		TerminalPayloadDigest:   runtimedomain.SHA256([]byte("terminal-output")),
		ProposalDigest:          runtimedomain.SHA256([]byte("proposal")),
		IdempotencyKey:          "settlement-key-1",
	}
}

/* ── AfterSettleHook ──────────────────────────────────────────────────── */

// TestAfterSettleHookFiresAfterTheRepositoryCommitsWithTheReceivedReceipt
// proves a hook sees the SAME proposal and receipt PrepareSettlement itself
// returns, and only after the repository call has already succeeded.
func TestAfterSettleHookFiresAfterTheRepositoryCommitsWithTheReceivedReceipt(t *testing.T) {
	repository := newMemorySettlementRepository()
	var mu sync.Mutex
	var gotProposal SettlementProposal
	var gotReceipt SettlementReceipt
	fired := make(chan struct{}, 1)
	hook := func(_ context.Context, proposal SettlementProposal, receipt SettlementReceipt) {
		mu.Lock()
		gotProposal, gotReceipt = proposal, receipt
		mu.Unlock()
		fired <- struct{}{}
	}
	service, err := NewSettlementService(repository, WithAfterSettleHooks(hook))
	if err != nil {
		t.Fatal(err)
	}

	proposal := validSettlementProposal()
	receipt, err := service.PrepareSettlement(context.Background(), proposal)
	if err != nil {
		t.Fatalf("PrepareSettlement: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("AfterSettleHook did not fire")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotProposal != proposal {
		t.Errorf("hook proposal = %+v, want %+v", gotProposal, proposal)
	}
	if gotReceipt != receipt {
		t.Errorf("hook receipt = %+v, want %+v", gotReceipt, receipt)
	}
}

// TestAfterSettleHookFailureCannotAffectSettlement is the load-bearing
// safety proof the task asks for: a hook that panics, and a second hook
// registered alongside it, must not change PrepareSettlement's own return
// value, must not stop the OTHER hook from running, and must not crash the
// test process.
func TestAfterSettleHookFailureCannotAffectSettlement(t *testing.T) {
	repository := newMemorySettlementRepository()
	panicking := func(context.Context, SettlementProposal, SettlementReceipt) {
		panic("AfterSettleHook exploded")
	}
	sawSecondHook := make(chan struct{}, 1)
	secondHook := func(context.Context, SettlementProposal, SettlementReceipt) {
		sawSecondHook <- struct{}{}
	}
	service, err := NewSettlementService(repository, WithAfterSettleHooks(panicking, secondHook))
	if err != nil {
		t.Fatal(err)
	}

	proposal := validSettlementProposal()
	receipt, err := service.PrepareSettlement(context.Background(), proposal)
	if err != nil {
		t.Fatalf("a panicking AfterSettleHook changed PrepareSettlement's error: %v", err)
	}
	if receipt.ID == "" || receipt.Outcome != proposal.Outcome {
		t.Fatalf("a panicking AfterSettleHook changed the receipt: %+v", receipt)
	}

	select {
	case <-sawSecondHook:
	case <-time.After(2 * time.Second):
		t.Fatal("the second AfterSettleHook did not run — one hook panicking must not stop its siblings")
	}

	// A settlement replay (the same idempotent proposal again) must ALSO
	// still work — proving the panic left no state behind that would wedge
	// a later, unrelated call.
	if _, err := service.PrepareSettlement(context.Background(), proposal); err != nil {
		t.Fatalf("PrepareSettlement after a panicking hook: %v", err)
	}
}

// TestAfterSettleHookFiresOnAnIdempotentReplayToo documents the contract
// pipelineruns.NewSettlementHook's own doc comment relies on: a hook cannot
// distinguish a fresh settlement from a replay of one already settled, and
// must handle that itself (ClaimForEvent's job, not this package's).
func TestAfterSettleHookFiresOnAnIdempotentReplayToo(t *testing.T) {
	repository := newMemorySettlementRepository()
	var calls int32
	var mu sync.Mutex
	done := make(chan struct{}, 2)
	hook := func(context.Context, SettlementProposal, SettlementReceipt) {
		mu.Lock()
		calls++
		mu.Unlock()
		done <- struct{}{}
	}
	service, err := NewSettlementService(repository, WithAfterSettleHooks(hook))
	if err != nil {
		t.Fatal(err)
	}
	proposal := validSettlementProposal()

	if _, err := service.PrepareSettlement(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareSettlement(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("hook fired %d times before the deadline, want 2", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("hook fired %d times for one fresh + one replayed settlement, want 2", calls)
	}
}

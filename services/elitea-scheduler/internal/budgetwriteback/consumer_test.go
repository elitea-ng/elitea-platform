package budgetwriteback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMessage is a Message seam impl with ack/nak/term counters.
type fakeMessage struct {
	data      string
	settleErr error // returned by Ack/Nak/Term to exercise the settle-failure log path
	acks      atomic.Int32
	naks      atomic.Int32
	term      atomic.Int32
}

func msg(d BudgetDelta) *fakeMessage {
	raw, _ := json.Marshal(d)
	return &fakeMessage{data: string(raw)}
}

func rawMsg(s string) *fakeMessage { return &fakeMessage{data: s} }

func (m *fakeMessage) Data() []byte { return []byte(m.data) }
func (m *fakeMessage) Ack() error   { m.acks.Add(1); return m.settleErr }
func (m *fakeMessage) Nak() error   { m.naks.Add(1); return m.settleErr }
func (m *fakeMessage) Term() error  { m.term.Add(1); return m.settleErr }

// scriptedFetcher yields queued batches one per Fetch call, then blocks on ctx
// so Run only makes forward progress on the scripted batches.
type scriptedFetcher struct {
	mu      sync.Mutex
	batches [][]Message
	err     error
}

func (f *scriptedFetcher) Fetch(ctx context.Context) ([]Message, error) {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.err = nil
		f.mu.Unlock()
		return nil, err
	}
	if len(f.batches) > 0 {
		b := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()
	// Nothing left: block until cancelled so Run exits cleanly.
	<-ctx.Done()
	return nil, ctx.Err()
}

// --- processBatch (pure, no loop) --------------------------------------------

func newConsumerWith(tx *fakeTx) (*Consumer, *fakeTx) {
	if tx == nil {
		tx = &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	}
	c := NewConsumer(nil, NewStore(&fakeDB{tx: tx}), quietLogger())
	return c, tx
}

func TestProcessBatch_AppliesAndAcks(t *testing.T) {
	c, _ := newConsumerWith(nil)
	m := msg(validDelta())
	c.processBatch(context.Background(), []Message{m})
	if m.acks.Load() != 1 {
		t.Errorf("acks = %d, want 1", m.acks.Load())
	}
	if m.naks.Load() != 0 || m.term.Load() != 0 {
		t.Errorf("unexpected nak/term: nak=%d term=%d", m.naks.Load(), m.term.Load())
	}
}

func TestProcessBatch_CoalescesSameKeyIntoOneTx(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	c, _ := newConsumerWith(tx)

	a := validDelta()
	b := validDelta()
	b.EventID = "22222222-2222-2222-2222-222222222222"
	b.DeltaNanoUSD = 500_000_000
	ma, mb := msg(a), msg(b)

	c.processBatch(context.Background(), []Message{ma, mb})

	// Both messages acked; a single coalesced UPSERT carries the summed delta.
	if ma.acks.Load() != 1 || mb.acks.Load() != 1 {
		t.Errorf("both messages must ack: a=%d b=%d", ma.acks.Load(), mb.acks.Load())
	}
	if got := tx.upsertArgs[6]; got != int64(3_000_000_000) {
		t.Errorf("coalesced UPSERT sum = %v, want 3000000000", got)
	}
}

func TestProcessBatch_DistinctKeysApplySeparately(t *testing.T) {
	// Each distinct key-group MUST get its OWN transaction. Mint a fresh fakeTx
	// per Begin and assert per-group isolation: exactly two txs, each committed
	// exactly once, each with its own single dedup-probe + upsert. A shared tx
	// would collapse these and hide cross-group contamination.
	db := &fakeDB{newTx: func() *fakeTx {
		return &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	}}
	c := NewConsumer(nil, NewStore(db), quietLogger())

	a := validDelta()
	b := validDelta()
	b.EventID = "22222222-2222-2222-2222-222222222222"
	b.ScopeID = "43" // different key ⇒ separate group ⇒ separate tx
	b.ProjectID = 43
	ma, mb := msg(a), msg(b)

	c.processBatch(context.Background(), []Message{ma, mb})

	if ma.acks.Load() != 1 || mb.acks.Load() != 1 {
		t.Errorf("distinct-key messages must both ack: a=%d b=%d", ma.acks.Load(), mb.acks.Load())
	}
	if len(db.txs) != 2 {
		t.Fatalf("distinct key-groups must each Begin their own tx: got %d txs, want 2", len(db.txs))
	}
	for i, tx := range db.txs {
		if !tx.committed {
			t.Errorf("tx[%d] must be committed", i)
		}
		if tx.rolledBack {
			t.Errorf("tx[%d] must NOT be rolled back", i)
		}
		if len(tx.dedupProbes) != 1 {
			t.Errorf("tx[%d] dedup probes = %d, want 1 (each group probes only its own delta)", i, len(tx.dedupProbes))
		}
		if !tx.upsertRan {
			t.Errorf("tx[%d] must run its own upsert", i)
		}
	}
}

func TestProcessBatch_UndecodableIsTermed(t *testing.T) {
	c, tx := newConsumerWith(nil)
	m := rawMsg("{not valid json")
	c.processBatch(context.Background(), []Message{m})
	if m.term.Load() != 1 {
		t.Errorf("poison JSON must be Term'd, got term=%d", m.term.Load())
	}
	if m.acks.Load() != 0 || m.naks.Load() != 0 {
		t.Error("poison message must not ack/nak")
	}
	if tx.upsertRan {
		t.Error("poison message must not reach the store")
	}
}

func TestProcessBatch_InvalidDeltaIsTermed(t *testing.T) {
	c, _ := newConsumerWith(nil)
	bad := validDelta()
	bad.Scope = "" // fails validate()
	m := msg(bad)
	c.processBatch(context.Background(), []Message{m})
	if m.term.Load() != 1 {
		t.Errorf("invalid delta must be Term'd, got term=%d", m.term.Load())
	}
}

func TestProcessBatch_DeferredIsNakd(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 0} // outage row
	c, _ := newConsumerWith(tx)
	m := msg(validDelta())
	c.processBatch(context.Background(), []Message{m})
	if m.naks.Load() != 1 {
		t.Errorf("deferred group must NAK for redelivery, got nak=%d", m.naks.Load())
	}
	if m.acks.Load() != 0 {
		t.Error("deferred group must not ack")
	}
}

func TestProcessBatch_TransientErrorIsNakd(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertErr: errors.New("deadlock")}
	c, _ := newConsumerWith(tx)
	m := msg(validDelta())
	c.processBatch(context.Background(), []Message{m})
	if m.naks.Load() != 1 {
		t.Errorf("transient failure must NAK, got nak=%d", m.naks.Load())
	}
}

func TestProcessBatch_SettleErrorsAreTolerated(t *testing.T) {
	// Ack/Nak/Term returning an error is logged, not fatal: the batch still
	// completes and every message's settle method is invoked exactly once.
	settleErr := errors.New("connection lost")

	// applied → Ack error path
	okTx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	cOK, _ := newConsumerWith(okTx)
	mAck := msg(validDelta())
	mAck.settleErr = settleErr
	cOK.processBatch(context.Background(), []Message{mAck})
	if mAck.acks.Load() != 1 {
		t.Errorf("ack attempted despite error: acks=%d", mAck.acks.Load())
	}

	// deferred → Nak error path
	deferTx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 0}
	cDef, _ := newConsumerWith(deferTx)
	mNak := msg(validDelta())
	mNak.settleErr = settleErr
	cDef.processBatch(context.Background(), []Message{mNak})
	if mNak.naks.Load() != 1 {
		t.Errorf("nak attempted despite error: naks=%d", mNak.naks.Load())
	}

	// poison → Term error path
	cTerm, _ := newConsumerWith(nil)
	mTerm := rawMsg("{bad")
	mTerm.settleErr = settleErr
	cTerm.processBatch(context.Background(), []Message{mTerm})
	if mTerm.term.Load() != 1 {
		t.Errorf("term attempted despite error: term=%d", mTerm.term.Load())
	}
}

func TestNewConsumer_NilLoggerUsesDefault(t *testing.T) {
	c := NewConsumer(nil, NewStore(&fakeDB{tx: &fakeTx{}}), nil)
	if c.logger == nil {
		t.Fatal("nil logger must fall back to slog.Default()")
	}
}

// --- Run (drain loop) --------------------------------------------------------

func TestRun_DrainsBatchesThenStopsOnCancel(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	m := msg(validDelta())
	f := &scriptedFetcher{batches: [][]Message{{m}}}
	c := NewConsumer(f, NewStore(&fakeDB{tx: tx}), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	// Wait for the scripted batch to be applied.
	waitFor(t, func() bool { return m.acks.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestRun_FetchErrorBacksOffThenContinues(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	m := msg(validDelta())
	// First Fetch errors; the loop backs off (1s) then the next Fetch returns
	// the batch. Use a short-circuit: seed err AND a batch.
	f := &scriptedFetcher{err: errors.New("stream unavailable"), batches: [][]Message{{m}}}
	c := NewConsumer(f, NewStore(&fakeDB{tx: tx}), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	// The 1s backoff means this takes just over a second.
	waitForTimeout(t, func() bool { return m.acks.Load() == 1 }, 3*time.Second)
	cancel()
	<-done
}

// errThenCancelFetcher returns an error on first Fetch, cancelling ctx as it
// does so — exercising the "ctx cancelled after fetch error" early return and
// the backoff-select ctx.Done() path in Run.
type errThenCancelFetcher struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (f *errThenCancelFetcher) Fetch(ctx context.Context) ([]Message, error) {
	f.calls.Add(1)
	f.cancel()
	return nil, errors.New("stream gone")
}

func TestRun_FetchErrorWithCancelledCtxReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &errThenCancelFetcher{cancel: cancel}
	c := NewConsumer(f, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run should return once ctx is cancelled after a fetch error")
	}
	if f.calls.Load() == 0 {
		t.Error("expected at least one Fetch call")
	}
}

func TestRun_IdleEmptyBatchContinues(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	m := msg(validDelta())
	// First batch is empty (idle stream → continue), then the real batch.
	f := &scriptedFetcher{batches: [][]Message{{}, {m}}}
	c := NewConsumer(f, NewStore(&fakeDB{tx: tx}), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, func() bool { return m.acks.Load() == 1 })
	cancel()
	<-done
}

// alwaysErrFetcher errors on every Fetch with ctx still live, so Run enters the
// 1s backoff select; the test cancels during that window to exercise the
// select's ctx.Done() early return (not the top-of-loop or post-fetch check).
type alwaysErrFetcher struct{ calls atomic.Int32 }

func (f *alwaysErrFetcher) Fetch(context.Context) ([]Message, error) {
	f.calls.Add(1)
	return nil, errors.New("stream unavailable")
}

func TestRun_CancelDuringBackoffReturns(t *testing.T) {
	f := &alwaysErrFetcher{}
	c := NewConsumer(f, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	// Let the first Fetch error land and Run enter the backoff select.
	waitFor(t, func() bool { return f.calls.Load() >= 1 })
	cancel() // lands during the 1s backoff window
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run should return when ctx is cancelled during backoff")
	}
}

func TestRun_StopsImmediatelyIfCtxAlreadyCancelled(t *testing.T) {
	f := &scriptedFetcher{}
	c := NewConsumer(f, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run should return promptly when ctx already cancelled")
	}
}

func TestVerifyConsumer(t *testing.T) {
	good := jetstream.ConsumerConfig{Durable: DurableName, FilterSubject: DeltaSubject, AckPolicy: jetstream.AckExplicitPolicy}
	if err := verifyConsumer(good); err != nil {
		t.Fatalf("the bootstrap's consumer was refused: %v", err)
	}
	multi := good
	multi.FilterSubject, multi.FilterSubjects = "", []string{DeltaSubject}
	if err := verifyConsumer(multi); err != nil {
		t.Errorf("FilterSubjects=[%s] was refused: %v", DeltaSubject, err)
	}
	for name, mutate := range map[string]func(*jetstream.ConsumerConfig){
		"push":         func(c *jetstream.ConsumerConfig) { c.DeliverSubject = "gateway.events.project.1.events" },
		"wider filter": func(c *jetstream.ConsumerConfig) { c.FilterSubject = "gateway.>" },
		"no filter":    func(c *jetstream.ConsumerConfig) { c.FilterSubject = "" },
		"two filters":  func(c *jetstream.ConsumerConfig) { c.FilterSubjects = []string{"x"} },
		"ack none":     func(c *jetstream.ConsumerConfig) { c.AckPolicy = jetstream.AckNonePolicy },
		"ack all":      func(c *jetstream.ConsumerConfig) { c.AckPolicy = jetstream.AckAllPolicy },
	} {
		c := good
		mutate(&c)
		if err := verifyConsumer(c); err == nil {
			t.Errorf("%s: verifyConsumer accepted it", name)
		}
	}
}

func TestConfigWithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.BatchSize != defaultBatchSize || got.FetchWait != defaultFetchWait {
		t.Errorf("zero Config did not fill defaults: %+v", got)
	}
	// Non-zero values are preserved.
	custom := Config{BatchSize: 10, FetchWait: time.Second}
	if custom.withDefaults() != custom {
		t.Error("non-zero Config must be preserved by withDefaults")
	}
}

// --- helpers -----------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	waitForTimeout(t, cond, 2*time.Second)
}

func waitForTimeout(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition not met within timeout")
		case <-tick.C:
		}
	}
}

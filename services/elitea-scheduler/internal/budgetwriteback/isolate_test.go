package budgetwriteback

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
)

// isolate_test.go — [human decision] 2026-10-16, "Isolate, then drop".

func deltaWithEvent(id string, nano int64) BudgetDelta {
	d := validDelta()
	d.EventID = id
	d.DeltaNanoUSD = nano
	d.Usage = &UsageDimensions{Provider: "openai", Model: "gpt-4o", PromptTokens: 1, CompletionTokens: 1, OccurredAtUnix: d.PeriodStart + 1}
	return d
}

const (
	goodA  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	poison = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	goodC  = "cccccccc-cccc-cccc-cccc-cccccccccccc"
)

// A coalesced group whose transaction fails is applied member by member. The
// one member that fails PERMANENTLY (a class-22 data error) is terminated,
// with the drop counted, and its neighbours apply and are acked.
func TestApplyGroup_APermanentFailureIsTermedAndItsNeighboursApply(t *testing.T) {
	before := metrics.droppedCount("permanent_apply_error", "22")
	permanent := &pgconn.PgError{Code: "22001", Message: "value too long"}
	db := &fakeDB{newTx: func() *fakeTx {
		return &fakeTx{
			alreadyApplied: map[string]bool{}, upsertAffected: 1,
			usageErrFor: map[string]error{poison: permanent},
		}
	}}
	c := NewConsumer(nil, NewStore(db), quietLogger())
	a, p, cc := msg(deltaWithEvent(goodA, 1)), msg(deltaWithEvent(poison, 2)), msg(deltaWithEvent(goodC, 3))
	c.processBatch(context.Background(), []Message{a, p, cc})

	if a.acks.Load() != 1 || cc.acks.Load() != 1 {
		t.Fatalf("neighbours: acks a=%d c=%d, want 1 each", a.acks.Load(), cc.acks.Load())
	}
	if p.term.Load() != 1 || p.acks.Load() != 0 || p.naks.Load() != 0 {
		t.Fatalf("poison: term=%d ack=%d nak=%d, want terminated only", p.term.Load(), p.acks.Load(), p.naks.Load())
	}
	if got := metrics.droppedCount("permanent_apply_error", "22") - before; got != 1 {
		t.Fatalf("budget_writeback_dropped_total{reason=permanent_apply_error,sqlstate_class=22} grew by %d, want 1", got)
	}
	body := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(body, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(body.Body.String(), `budget_writeback_dropped_total{reason="permanent_apply_error",sqlstate_class="22"}`) {
		t.Fatalf("/metrics does not expose the drop:\n%s", body.Body.String())
	}
	// 1 group transaction + 3 member transactions.
	if len(db.txs) != 4 {
		t.Fatalf("transactions = %d, want the group then one per member", len(db.txs))
	}
}

// A TRANSIENT failure is never a drop: the member is deferred with the
// backoff, whatever its group did.
func TestApplyGroup_ATransientFailureIsDeferredNotDropped(t *testing.T) {
	for _, transient := range []error{
		&pgconn.PgError{Code: "40001"}, // serialization
		&pgconn.PgError{Code: "53300"}, // too many connections
		&pgconn.PgError{Code: "57014"}, // query cancelled
		&pgconn.PgError{Code: "08006"}, // connection failure
		&pgconn.PgError{Code: "23505", ConstraintName: "processed_event_ids_pkey"},
		errors.New("no SQLSTATE at all"),
	} {
		tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1, usageErr: transient}
		c, _ := newConsumerWith(tx)
		m := msg(deltaWithEvent(goodA, 1))
		c.processBatch(context.Background(), []Message{m})
		if m.naks.Load() != 1 || m.term.Load() != 0 || len(m.delays) != 1 || m.delays[0] != 5*time.Second {
			t.Errorf("%v: nak=%d term=%d delays=%v, want one 5s deferral", transient, m.naks.Load(), m.term.Load(), m.delays)
		}
	}
}

func TestPermanentApplyFailure_Classes(t *testing.T) {
	for code, want := range map[string]bool{
		"22001": true, "22P02": true, "23502": true, "23503": true, "23505": true, "42703": true, "42P01": true,
		"40001": false, "40P01": false, "53200": false, "57014": false, "08006": false, "XX000": false,
	} {
		if _, got := permanentApplyFailure(&pgconn.PgError{Code: code}); got != want {
			t.Errorf("%s: permanent = %v, want %v", code, got, want)
		}
	}
	if _, got := permanentApplyFailure(&pgconn.PgError{Code: "23505", TableName: "processed_event_ids"}); got {
		t.Error("the dedup conflict is permanent")
	}
}

// The known trigger is gone: an over-long model name is trimmed to its
// VARCHAR width and the money lands.
func TestApply_AnOverLongModelNameIsTrimmedAndTheMoneyLands(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	c, _ := newConsumerWith(tx)
	d := deltaWithEvent(goodA, 7)
	d.Usage.Model = strings.Repeat("é", 200) // 400 bytes
	d.Usage.Provider = strings.Repeat("p", 100)
	d.Usage.ExecutionID = strings.Repeat("x", 300)
	m := msg(d)
	c.processBatch(context.Background(), []Message{m})
	if m.acks.Load() != 1 || !tx.upsertRan {
		t.Fatalf("ack=%d upsert=%v, want the money applied", m.acks.Load(), tx.upsertRan)
	}
	args := tx.usageInserts[0]
	provider, model, exec := args[3].(string), args[4].(string), args[11].(string)
	if len(provider) != usageProviderBytes || len(model) > usageModelBytes || len(exec) != usageExecutionIDBytes {
		t.Fatalf("provider=%d model=%d exec=%d bytes", len(provider), len(model), len(exec))
	}
	if !strings.HasPrefix(d.Usage.Model, model) || strings.ContainsRune(model, '\uFFFD') {
		t.Fatalf("the model was not cut on a UTF-8 boundary: %q", model)
	}
}

// A message whose metadata cannot be read is still deferred with the first
// backoff step, never a bare Nak.
func TestNakAll_UnreadableMetadataStillBacksOff(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 0}
	c, _ := newConsumerWith(tx)
	m := msg(validDelta())
	m.metadataErr = errors.New("not a JetStream message")
	c.processBatch(context.Background(), []Message{m})
	if len(m.delays) != 1 || m.delays[0] != 5*time.Second {
		t.Fatalf("delays = %v, want [5s]", m.delays)
	}
}

type infoSource struct{ info *jetstream.ConsumerInfo }

func (s infoSource) Info(context.Context) (*jetstream.ConsumerInfo, error) { return s.info, nil }

func TestConsumerInfo_GaugesAreRefreshedAndServed(t *testing.T) {
	c := NewConsumer(nil, nil, quietLogger())
	c.info = infoSource{&jetstream.ConsumerInfo{NumAckPending: 123, NumRedelivered: 45}}
	c.refreshConsumerInfoIfDue(context.Background())
	out := metrics.render()
	for _, want := range []string{"budget_writeback_num_ack_pending 123", "budget_writeback_num_redelivered 45"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, out)
		}
	}
	// Within the interval the server is not asked again.
	c.info = infoSource{&jetstream.ConsumerInfo{NumAckPending: 999}}
	c.refreshConsumerInfoIfDue(context.Background())
	if !strings.Contains(metrics.render(), "budget_writeback_num_ack_pending 123") {
		t.Fatal("the gauges were refreshed inside the interval")
	}
}

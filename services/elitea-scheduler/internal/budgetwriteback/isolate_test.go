package budgetwriteback

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
)

// isolate_test.go — [human decision] 2026-10-16, "Isolate, then drop",
// revised "Drop only 22/23, retry 42".

// ─── a stateful database ─────────────────────────────────────────────────────

// ledgerDB is a fake database with COMMITTED state: event-id claims and the
// accumulated money become visible only on Commit and are discarded on
// Rollback, as in Postgres. So a test can tell "applied once" from "applied
// twice" or "applied and rolled back", which a stateless fake cannot.
type ledgerDB struct {
	mu        sync.Mutex
	claimed   map[string]bool // gateway.processed_event_ids, committed
	money     int64           // gateway.llm_budget_accumulators, committed
	ledger    map[string]int  // gateway.llm_usage_events rows, committed
	failUsage map[string]error
	failAll   error // every statement of every transaction fails with it
	txs       []*ledgerTx
}

func newLedgerDB() *ledgerDB {
	return &ledgerDB{claimed: map[string]bool{}, ledger: map[string]int{}, failUsage: map[string]error{}}
}

func (d *ledgerDB) Begin(context.Context) (Tx, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx := &ledgerTx{db: d, claims: map[string]bool{}}
	d.txs = append(d.txs, tx)
	return tx, nil
}

func (d *ledgerDB) committedMoney() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.money
}

type ledgerTx struct {
	db         *ledgerDB
	claims     map[string]bool
	usage      []string
	money      int64
	committed  bool
	rolledBack bool
}

func (t *ledgerTx) QueryRow(_ context.Context, sql string, args ...any) Row {
	if t.db.failAll != nil {
		return fakeRow{scanErr: t.db.failAll}
	}
	if !strings.Contains(sql, "processed_event_ids") {
		return fakeRow{scanErr: errors.New("unexpected QueryRow sql")}
	}
	id, _ := args[0].(string)
	t.db.mu.Lock()
	taken := t.db.claimed[id]
	t.db.mu.Unlock()
	if taken || t.claims[id] {
		return fakeRow{scanErr: pgx.ErrNoRows}
	}
	t.claims[id] = true
	return fakeRow{returnID: id}
}

func (t *ledgerTx) ExecAffected(_ context.Context, sql string, args ...any) (int64, error) {
	if t.db.failAll != nil {
		return 0, t.db.failAll
	}
	switch {
	case strings.Contains(sql, "llm_usage_events"):
		id, _ := args[0].(string)
		if err := t.db.failUsage[id]; err != nil {
			return 0, err
		}
		t.usage = append(t.usage, id)
		return 1, nil
	case strings.Contains(sql, "llm_budget_accumulators"):
		t.money += args[6].(int64)
		return 1, nil
	}
	return 0, errors.New("unexpected Exec sql")
}

func (t *ledgerTx) Commit(context.Context) error {
	if t.rolledBack {
		return errors.New("commit after rollback")
	}
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	for id := range t.claims {
		t.db.claimed[id] = true
	}
	for _, id := range t.usage {
		t.db.ledger[id]++
	}
	t.db.money += t.money
	t.committed = true
	return nil
}

func (t *ledgerTx) Rollback(context.Context) error {
	if !t.committed {
		t.rolledBack = true
	}
	return nil
}

// ─── fixtures ───────────────────────────────────────────────────────────────

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

	nanoA      = int64(1_000)
	nanoPoison = int64(20_000)
	nanoC      = int64(300_000)
)

func threeMemberBatch() (a, p, c *fakeMessage) {
	return msg(deltaWithEvent(goodA, nanoA)), msg(deltaWithEvent(poison, nanoPoison)), msg(deltaWithEvent(goodC, nanoC))
}

// ─── isolation ──────────────────────────────────────────────────────────────

// A group that fails on ONE delta's data (class 22) is applied member by
// member. That member is terminated and counted; its neighbours apply ONCE,
// each in its own committed transaction, and the group's own transaction
// committed nothing.
func TestApplyGroup_APermanentFailureIsTermedAndItsNeighboursApply(t *testing.T) {
	before := metrics.droppedCount("permanent_apply_error", "22")
	db := newLedgerDB()
	db.failUsage[poison] = &pgconn.PgError{Code: "22001", Message: "value too long"}
	c := NewConsumer(nil, NewStore(db), quietLogger())
	a, p, cc := threeMemberBatch()
	c.processBatch(context.Background(), []Message{a, p, cc})

	if a.acks.Load() != 1 || cc.acks.Load() != 1 || a.naks.Load()+cc.naks.Load() != 0 {
		t.Fatalf("neighbours: acks a=%d c=%d naks=%d, want acked once each", a.acks.Load(), cc.acks.Load(), a.naks.Load()+cc.naks.Load())
	}
	if p.term.Load() != 1 || p.acks.Load() != 0 || p.naks.Load() != 0 {
		t.Fatalf("poison: term=%d ack=%d nak=%d, want terminated only", p.term.Load(), p.acks.Load(), p.naks.Load())
	}
	if len(db.txs) != 4 {
		t.Fatalf("transactions = %d, want the group, then one per member", len(db.txs))
	}
	if g := db.txs[0]; g.committed || !g.rolledBack {
		t.Fatalf("the group transaction: committed=%v rolledBack=%v, want rolled back only", g.committed, g.rolledBack)
	}
	for i, want := range []int64{nanoA, nanoC} {
		tx := db.txs[1+2*i]
		if !tx.committed || tx.money != want {
			t.Fatalf("member tx %d: committed=%v money=%d, want committed with %d", 1+2*i, tx.committed, tx.money, want)
		}
	}
	if tx := db.txs[2]; tx.committed {
		t.Fatal("the poison member's transaction committed")
	}
	if got := db.committedMoney(); got != nanoA+nanoC {
		t.Fatalf("committed money = %d, want exactly %d (goodA + goodC, once)", got, nanoA+nanoC)
	}
	if db.ledger[goodA] != 1 || db.ledger[goodC] != 1 || db.ledger[poison] != 0 {
		t.Fatalf("ledger rows = %v, want one each for the neighbours and none for the poison", db.ledger)
	}
	if got := metrics.droppedCount("permanent_apply_error", "22") - before; got != 1 {
		t.Fatalf("budget_writeback_dropped_total{reason=permanent_apply_error,sqlstate_class=22} grew by %d, want 1", got)
	}
	body := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(body, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(body.Body.String(), `budget_writeback_dropped_total{reason="permanent_apply_error",sqlstate_class="22"}`) {
		t.Fatalf("/metrics does not expose the drop:\n%s", body.Body.String())
	}
}

// A group that fails TRANSIENTLY is not isolated: one transaction, one 5s
// delayed Nak per member, no Term, no Ack, no drop counted.
func TestApplyGroup_ATransientGroupFailureIsDeferredInOneTransaction(t *testing.T) {
	for _, code := range []string{"08006", "40001", "40P01", "55P03", "57P01"} {
		before := metrics.totalDropped()
		db := newLedgerDB()
		db.failAll = &pgconn.PgError{Code: code}
		c := NewConsumer(nil, NewStore(db), quietLogger())
		a, p, cc := threeMemberBatch()
		c.processBatch(context.Background(), []Message{a, p, cc})
		if len(db.txs) != 1 {
			t.Errorf("%s: transactions = %d, want exactly 1 (no isolation on a transient error)", code, len(db.txs))
		}
		for name, m := range map[string]*fakeMessage{"a": a, "p": p, "c": cc} {
			if m.naks.Load() != 1 || len(m.delays) != 1 || m.delays[0] != 5*time.Second || m.term.Load() != 0 || m.acks.Load() != 0 {
				t.Errorf("%s/%s: nak=%d delays=%v term=%d ack=%d, want one 5s deferral", code, name, m.naks.Load(), m.delays, m.term.Load(), m.acks.Load())
			}
		}
		if metrics.totalDropped() != before || db.committedMoney() != 0 {
			t.Errorf("%s: a transient failure dropped or applied money", code)
		}
	}
}

// Class 42 is the environment, not a delta ([human decision] 2026-10-16,
// "Drop only 22/23, retry 42"): a scheduler started before the migrations, or
// a revoked grant, fails every member. None is terminated; all are deferred.
func TestApplyGroup_AnEnvironmentalFailureIsNeverDropped(t *testing.T) {
	for _, code := range []string{"42501", "42P01", "42703"} {
		before := metrics.totalDropped()
		db := newLedgerDB()
		db.failAll = &pgconn.PgError{Code: code}
		c := NewConsumer(nil, NewStore(db), quietLogger())
		a, p, cc := threeMemberBatch()
		c.processBatch(context.Background(), []Message{a, p, cc})
		for name, m := range map[string]*fakeMessage{"a": a, "p": p, "c": cc} {
			if m.term.Load() != 0 || m.naks.Load() != 1 || m.acks.Load() != 0 {
				t.Errorf("%s/%s: term=%d nak=%d ack=%d, want deferred, never terminated", code, name, m.term.Load(), m.naks.Load(), m.acks.Load())
			}
		}
		if len(db.txs) != 1 {
			t.Errorf("%s: transactions = %d, want 1 (no isolation)", code, len(db.txs))
		}
		if metrics.totalDropped() != before {
			t.Errorf("%s: an environmental failure was counted as a drop", code)
		}
	}
}

// A single delta that fails transiently is deferred, never dropped.
func TestApplyGroup_ATransientFailureIsDeferredNotDropped(t *testing.T) {
	for _, transient := range []error{
		&pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "53300"},
		&pgconn.PgError{Code: "57014"},
		&pgconn.PgError{Code: "08006"},
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
		"22001": true, "22P02": true, "23502": true, "23503": true, "23505": true,
		// Class 42 is environmental, never permanent.
		"42501": false, "42P01": false, "42703": false, "42P05": false, "42P10": false,
		"40001": false, "40P01": false, "55P03": false, "53200": false, "57014": false, "57P01": false,
		"08006": false, "XX000": false,
	} {
		if _, got := permanentApplyFailure(&pgconn.PgError{Code: code}); got != want {
			t.Errorf("%s: permanent = %v, want %v", code, got, want)
		}
	}
	if _, got := permanentApplyFailure(&pgconn.PgError{Code: "23505", TableName: "processed_event_ids"}); got {
		t.Error("the dedup conflict is permanent")
	}
	if !environmentalApplyFailure(&pgconn.PgError{Code: "42501"}) || environmentalApplyFailure(&pgconn.PgError{Code: "22001"}) {
		t.Error("environmentalApplyFailure misclassifies class 42 / class 22")
	}
}

// ─── truncation ─────────────────────────────────────────────────────────────

// The cut lands on a rune boundary for 2-, 3- and 4-byte runes straddling the
// limit, at the exact length.
func TestTruncateUTF8(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		limit int
		want  int // bytes kept
	}{
		{"short", "abc", 8, 3},
		{"exact", "abcd", 4, 4},
		{"ascii", "abcdef", 4, 4},
		{"2-byte straddle", "abc" + "é", 4, 3}, // é = 2 bytes at 3..4
		{"3-byte straddle", "ab" + "€", 4, 2},  // € = 3 bytes at 2..4
		{"4-byte straddle", "a" + "😀", 4, 1},   // 😀 = 4 bytes at 1..4
		{"4-byte fits", "😀" + "x", 4, 4},       // the rune ends at the limit
		{"3-byte boundary", "€€", 3, 3},
		{"empty", "", 4, 0},
	} {
		got := truncateUTF8(tc.in, tc.limit)
		if len(got) != tc.want || !utf8.ValidString(got) || !strings.HasPrefix(tc.in, got) {
			t.Errorf("%s: %q -> %q (%d bytes), want %d valid bytes", tc.name, tc.in, got, len(got), tc.want)
		}
	}
}

// The known trigger is gone: an over-long model name is trimmed to its
// VARCHAR width at a rune boundary, and the money lands.
func TestApply_AnOverLongModelNameIsTrimmedAndTheMoneyLands(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	c, _ := newConsumerWith(tx)
	d := deltaWithEvent(goodA, 7)
	d.Usage.Model = "m" + strings.Repeat("€", 60) // 181 bytes; a 3-byte rune straddles 128
	d.Usage.Provider = "pp" + strings.Repeat("😀", 20)
	d.Usage.ExecutionID = strings.Repeat("é", 100)
	m := msg(d)
	c.processBatch(context.Background(), []Message{m})
	if m.acks.Load() != 1 || !tx.upsertRan {
		t.Fatalf("ack=%d upsert=%v, want the money applied", m.acks.Load(), tx.upsertRan)
	}
	args := tx.usageInserts[0]
	for _, tc := range []struct {
		name string
		got  string
		want int
	}{
		{"provider", args[3].(string), 62}, // "pp" + 15 x 4-byte rune
		{"model", args[4].(string), 127},   // "m" + 42 x 3-byte rune
		{"execution_id", args[11].(string), 128},
	} {
		if len(tc.got) != tc.want || !utf8.ValidString(tc.got) {
			t.Errorf("%s: %d bytes (valid=%v), want %d valid bytes", tc.name, len(tc.got), utf8.ValidString(tc.got), tc.want)
		}
	}
}

// ─── backoff and gauges ─────────────────────────────────────────────────────

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

func TestConsumerInfo_GaugesAreRefreshedServedAndWithdrawn(t *testing.T) {
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
	// A drain loop that stops withdraws them.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
	if strings.Contains(metrics.render(), "budget_writeback_num_ack_pending") {
		t.Fatal("the gauges outlived the drain loop")
	}
}

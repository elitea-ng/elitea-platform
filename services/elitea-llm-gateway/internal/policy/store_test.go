package policy

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// fakeRows is a minimal pgx.Rows over a fixed set of governance rows.
type fakeRows struct {
	rows    [][]any
	i       int
	scanErr error
	rowsErr error
}

func (f *fakeRows) Next() bool                                   { f.i++; return f.i <= len(f.rows) }
func (f *fakeRows) Close()                                       {}
func (f *fakeRows) Err() error                                   { return f.rowsErr }
func (f *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (f *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (f *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (f *fakeRows) RawValues() [][]byte                          { return nil }
func (f *fakeRows) Conn() *pgx.Conn                              { return nil }

// TypeMap joined the pgx.Rows interface in pgx v5.11.0. A fake that omits it
// no longer compiles; nil is correct here because nothing in this package
// decodes through the type map — Scan below writes the fixture values
// directly.
func (f *fakeRows) TypeMap() *pgtype.Map { return nil }

func (f *fakeRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	src := f.rows[f.i-1]
	*(dest[0].(*string)) = src[0].(string)
	*(dest[1].(*string)) = src[1].(string)
	*(dest[2].(*string)) = src[2].(string)
	*(dest[3].(*string)) = src[3].(string)
	*(dest[4].(*[]byte)) = src[4].([]byte)
	*(dest[5].(*bool)) = src[5].(bool)
	return nil
}

type fakeDB struct {
	rows  [][]any
	err   error
	calls int
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &fakeRows{rows: f.rows}, nil
}

func dbRow(id, typ, name, data string) []any {
	return []any{id, typ, name, "governance", []byte(data), true}
}

func newTestStore(db querier) *Store {
	return NewStore(Config{DB: db, Logger: quietLogger(), Now: func() time.Time { return testNow }})
}

func TestStoreLoadsAndPublishes(t *testing.T) {
	t.Parallel()

	db := &fakeDB{rows: [][]any{
		dbRow("1", TypeModelConfig, "allow", `{"scope":{"models":["gpt-4o"]}}`),
	}}
	s := newTestStore(db)

	if s.Current() != Empty {
		t.Error("a store holds the Empty snapshot before its first load")
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := s.Current().Diagnostics().ModelConfigs; got != 1 {
		t.Errorf("loaded %d model configs, want 1", got)
	}
	st := s.Status()
	if !st.HasDatabase || st.Error != "" || st.LastSuccess == "" {
		t.Errorf("status after a good load = %+v", st)
	}
}

// TestFailedRefreshKeepsThePreviousSnapshot is the deliberate staleness rule.
// Dropping to Empty on a database blip would lift every allowlist and every
// rate limit at the worst possible moment.
func TestFailedRefreshKeepsThePreviousSnapshot(t *testing.T) {
	t.Parallel()

	db := &fakeDB{rows: [][]any{
		dbRow("1", TypeModelConfig, "allow", `{"scope":{"models":["gpt-4o"]}}`),
	}}
	s := newTestStore(db)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	db.err = errors.New("connection refused")
	if err := s.Load(context.Background()); err == nil {
		t.Fatal("a failing Load reported success")
	}
	if got := s.Current().Diagnostics().ModelConfigs; got != 1 {
		t.Errorf("the previous snapshot was dropped on a failed refresh (%d model configs)", got)
	}
	st := s.Status()
	if st.Error == "" {
		t.Error("the refresh failure is not reported; the staleness would be invisible")
	}
	if st.LastSuccess == "" {
		t.Error("the last good load is not reported, so an operator cannot tell how stale the snapshot is")
	}
}

func TestStoreWithNoDatabase(t *testing.T) {
	t.Parallel()

	s := NewStore(Config{Logger: quietLogger()})
	if err := s.Load(context.Background()); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("Load without a pool = %v, want ErrNoDatabase", err)
	}
	if s.Current() != Empty {
		t.Error("a store with no pool must serve the Empty snapshot")
	}
	if s.Status().HasDatabase {
		t.Error("Status reports a database this store does not have")
	}
}

// TestMalformedJSONBRowIsRejectedByNameNotDropped keeps a row that fails to
// unmarshal visible. A row that vanished during scan is the failure this whole
// feature exists to end.
func TestMalformedJSONBRowIsRejectedByNameNotDropped(t *testing.T) {
	t.Parallel()

	db := &fakeDB{rows: [][]any{
		dbRow("bad", TypeRoutingRule, "broken", `{not json`),
	}}
	s := newTestStore(db)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	rejected := s.Current().Diagnostics().Rejected
	if len(rejected) != 1 || rejected[0].ID != "bad" {
		t.Fatalf("the malformed row was not reported: %+v", rejected)
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	t.Parallel()

	var s *Store
	if s.Current() != Empty {
		t.Error("a nil store did not answer Empty")
	}
	if !reflect.DeepEqual(s.Status(), Status{}) {
		t.Error("a nil store returned a populated status")
	}
}

// TestStartLoadsBeforeReturning is what makes the gateway enforce authored
// definitions on its FIRST request rather than after the first tick.
func TestStartLoadsBeforeReturning(t *testing.T) {
	t.Parallel()

	db := &fakeDB{rows: [][]any{
		dbRow("1", TypeRateLimit, "cap", `{"rate_limit":{"requests_per_min":10}}`),
	}}
	s := NewStore(Config{
		DB: db, Logger: quietLogger(),
		RefreshInterval: time.Hour, // long enough that only the initial load can have run
		Now:             func() time.Time { return testNow },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	if _, ok := s.Current().RateLimit(Subject{ProjectID: 1}); !ok {
		t.Error("Start returned before the first load completed")
	}
}

func TestDefaultRefreshIntervalApplies(t *testing.T) {
	t.Parallel()

	s := NewStore(Config{Logger: quietLogger()})
	if s.Status().RefreshInterval != DefaultRefreshInterval.String() {
		t.Errorf("RefreshInterval = %q, want the default", s.Status().RefreshInterval)
	}
}

package securityaudit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeTx answers the appender's two statements. Every other pgx.Tx method
// panics through the nil embedded interface, so an unexpected call fails.
type fakeTx struct {
	pgx.Tx
	headSeq   int64
	headHash  []byte
	headErr   error
	insertErr error
	moved     int64

	calls      []string
	insertArgs []any
	execs      []string
	rolledBack bool
	committed  bool
}

type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "security_audit_heads AS head"):
		f.calls = append(f.calls, "head")
		return fakeRow{scan: func(dest ...any) error {
			if f.headErr != nil {
				return f.headErr
			}
			*dest[0].(*int64) = f.headSeq
			*dest[1].(*[]byte) = f.headHash
			return nil
		}}
	case strings.Contains(sql, "INSERT INTO elitea_runtime.security_audit_outbox"):
		f.calls = append(f.calls, "insert")
		f.insertArgs = args
		return fakeRow{scan: func(dest ...any) error {
			if f.insertErr != nil {
				return f.insertErr
			}
			*dest[0].(*int64) = 900
			*dest[1].(*time.Time) = time.Unix(0, 0)
			*dest[2].(*int64) = f.moved
			return nil
		}}
	}
	panic("unexpected statement: " + sql)
}

func (f *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }
func (f *fakeTx) Commit(context.Context) error   { f.committed = true; return nil }

func newTestAppender(t *testing.T) *PostgresAppender {
	t.Helper()
	appender, err := NewPostgresAppender("elitea-main/test")
	if err != nil {
		t.Fatal(err)
	}
	appender.now = func() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.FixedZone("x", 3600)) }
	appender.newID = func() (uuid.UUID, error) { return uuid.MustParse("0192f0c1-7b6e-7c3a-8f00-0000000000aa"), nil }
	return appender
}

func TestAppendChainsAfterTheHead(t *testing.T) {
	prev := Hash{9, 9, 9}
	tx := &fakeTx{headSeq: 4, headHash: prev[:], moved: 1}
	event := sampleEvent()
	event.ID, event.Source, event.Time = "", "", time.Time{}
	record, err := newTestAppender(t).AppendRecord(context.Background(), tx, event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tx.calls, ",") != "head,insert" {
		t.Fatalf("calls %v", tx.calls)
	}
	if record.PartitionSeq != 5 || record.PartitionKey != "42" || record.Seq != 900 || record.PrevHash != prev {
		t.Fatalf("record %+v", record)
	}
	if record.EventID != "0192f0c1-7b6e-7c3a-8f00-0000000000aa" {
		t.Fatalf("id %q", record.EventID)
	}
	if record.Hash != ChainHash(prev, record.Event) {
		t.Fatal("hash is not sha256(prev || canonical)")
	}
	if !bytes.Contains(record.Event, []byte(`"source":"elitea-main/test"`)) ||
		!bytes.Contains(record.Event, []byte(`"time":"2026-10-25T23:00:00Z"`)) {
		t.Fatalf("defaults not applied: %s", record.Event)
	}
	if tx.insertArgs[1] != int64(5) || tx.insertArgs[4] != string(record.Event) {
		t.Fatalf("insert args %v", tx.insertArgs)
	}
}

func TestAppendRequiresATransaction(t *testing.T) {
	if err := newTestAppender(t).Append(context.Background(), nil, sampleEvent()); !errors.Is(err, ErrNoTransaction) {
		t.Fatalf("err = %v", err)
	}
}

func TestAppendRefusesAnInvalidEventBeforeAnyStatement(t *testing.T) {
	tx := &fakeTx{headHash: ZeroHash[:], moved: 1}
	event := sampleEvent()
	event.PrincipalRef = "Bearer x"
	if err := newTestAppender(t).Append(context.Background(), tx, event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("err = %v", err)
	}
	if len(tx.calls) != 0 {
		t.Fatalf("statements ran: %v", tx.calls)
	}
}

func TestAppendPropagatesEveryDatabaseError(t *testing.T) {
	boom := errors.New("connection reset")
	for name, tx := range map[string]*fakeTx{
		"head":       {headErr: boom},
		"insert":     {headHash: ZeroHash[:], insertErr: boom},
		"head moved": {headHash: ZeroHash[:], moved: 0},
		"bad head":   {headHash: []byte{1}},
	} {
		t.Run(name, func(t *testing.T) {
			err := newTestAppender(t).Append(context.Background(), tx, sampleEvent())
			if !errors.Is(err, ErrAppendFailed) {
				t.Fatalf("err = %v, want ErrAppendFailed", err)
			}
		})
	}
}

// governedRevoke stands in for a broker operation: it makes a change and
// records it in the same transaction, and it fails when the record fails.
func governedRevoke(ctx context.Context, begin func() pgx.Tx, appender Appender) (err error) {
	tx := begin()
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "UPDATE grants SET revoked = true"); err != nil {
		return err
	}
	event := sampleEvent()
	event.Type, event.Action, event.Effect = TypeGrantChanged, "grant.revoke", ""
	if err = appender.Append(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestAFailedAppendFailsAndRollsBackTheGovernedOperation(t *testing.T) {
	tx := &fakeTx{}
	err := governedRevoke(context.Background(), func() pgx.Tx { return tx }, FailingAppender{})
	if !errors.Is(err, ErrAppendFailed) {
		t.Fatalf("err = %v", err)
	}
	if !tx.rolledBack || tx.committed {
		t.Fatalf("rolledBack=%t committed=%t: the change must not commit without its evidence", tx.rolledBack, tx.committed)
	}

	ok := &fakeTx{headHash: ZeroHash[:], moved: 1}
	if err := governedRevoke(context.Background(), func() pgx.Tx { return ok }, newTestAppender(t)); err != nil {
		t.Fatal(err)
	}
	if !ok.committed || ok.rolledBack {
		t.Fatal("the successful path did not commit")
	}
}

func TestNewPostgresAppenderRefusesABadSource(t *testing.T) {
	for _, source := range []string{"", "  ", "a\nb"} {
		if _, err := NewPostgresAppender(source); err == nil {
			t.Errorf("source %q accepted", source)
		}
	}
}

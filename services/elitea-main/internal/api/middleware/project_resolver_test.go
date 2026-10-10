package middleware

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeRow implements pgx.Row for a single canned result or error.
type fakeRow struct {
	err  error
	vals []any
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		if i >= len(r.vals) {
			break
		}
		switch d := dest[i].(type) {
		case *int:
			if v, ok := r.vals[i].(int); ok {
				*d = v
			}
		case *bool:
			if v, ok := r.vals[i].(bool); ok {
				*d = v
			}
		case *string:
			if v, ok := r.vals[i].(string); ok {
				*d = v
			}
		}
	}
	return nil
}

// fakeQuerier routes QueryRow calls to a handler keyed on a substring of the SQL.
type fakeQuerier struct {
	handler func(sql string, args ...any) pgx.Row
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return q.handler(sql, args...)
}

func newResolver(handler func(sql string, args ...any) pgx.Row) *DBPersonalProjectResolver {
	return &DBPersonalProjectResolver{pool: &fakeQuerier{handler: handler}}
}

func TestPersonalProjectID_EmptyUser(t *testing.T) {
	r := newResolver(func(string, ...any) pgx.Row { t.Fatal("must not query"); return nil })
	id, err := r.PersonalProjectID(context.Background(), "")
	if err != nil || id != 0 {
		t.Fatalf("got (%d,%v), want (0,nil)", id, err)
	}
}

func TestPersonalProjectID_NamedProjectMember(t *testing.T) {
	r := newResolver(func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{vals: []any{77}}
		case strings.Contains(sql, "project_user_role"):
			return fakeRow{vals: []any{true}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 77 {
		t.Errorf("got %d, want 77", id)
	}
}

func TestPersonalProjectID_NamedProjectNotMember_FallsBackToEmail(t *testing.T) {
	r := newResolver(func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{vals: []any{77}}
		case strings.Contains(sql, "project_user_role"):
			return fakeRow{vals: []any{false}} // not a member
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{vals: []any{"system_user_9@centry.user"}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 9 {
		t.Errorf("expected email-fallback id 9, got %d", id)
	}
}

func TestPersonalProjectID_NoNamedProject_EmailFallback(t *testing.T) {
	r := newResolver(func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{vals: []any{"system_user_42@centry.user"}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 42 {
		t.Errorf("expected 42, got %d", id)
	}
}

func TestPersonalProjectID_NoNamedProject_NonSystemEmail(t *testing.T) {
	r := newResolver(func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{vals: []any{"alice@example.com"}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0 {
		t.Errorf("expected 0 for non-system email, got %d", id)
	}
}

func TestPersonalProjectID_ProjectQueryError(t *testing.T) {
	wantErr := errors.New("connection reset")
	r := newResolver(func(sql string, args ...any) pgx.Row {
		return fakeRow{err: wantErr}
	})
	_, err := r.PersonalProjectID(context.Background(), "5")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped connection error, got %v", err)
	}
}

func TestPersonalProjectID_NonNumericUserID_EmailFallbackSkipped(t *testing.T) {
	// A non-numeric user id cannot be a member and has no numeric email row;
	// userInProject returns (false,nil) then the email lookup is attempted with
	// a non-numeric id → 0,nil.
	r := newResolver(func(sql string, args ...any) pgx.Row {
		if strings.Contains(sql, "FROM centry.project") {
			return fakeRow{vals: []any{77}}
		}
		t.Fatalf("unexpected query for non-numeric uid: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0 {
		t.Errorf("expected 0 for non-numeric uid, got %d", id)
	}
}

func TestPersonalProjectID_UserNotFound(t *testing.T) {
	r := newResolver(func(sql string, args ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{err: pgx.ErrNoRows}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	})
	id, err := r.PersonalProjectID(context.Background(), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0 {
		t.Errorf("expected 0 when user missing, got %d", id)
	}
}

func TestPersonalProjectID_NilPool(t *testing.T) {
	r := &DBPersonalProjectResolver{pool: nil}
	_, err := r.PersonalProjectID(context.Background(), "5")
	if err == nil {
		t.Fatal("expected error for nil pool")
	}
}

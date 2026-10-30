package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// countRow scans one count(*) value.
type countRow struct{ n int64 }

func (r countRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.n
	return nil
}

// throttleQuerier answers the two existence counts and the insert.
type throttleQuerier struct {
	fakeQuerier
	users, agents int64
	agentErr      error
	countSQL      []string
}

func (q *throttleQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "auth_core__user"):
		q.countSQL = append(q.countSQL, sql)
		return countRow{n: q.users}
	case strings.Contains(sql, ".applications"):
		q.countSQL = append(q.countSQL, sql)
		if q.agentErr != nil {
			return &fakeRow{scanErr: q.agentErr}
		}
		return countRow{n: q.agents}
	}
	return q.fakeQuerier.QueryRow(ctx, sql, args...)
}

func throttleBody(t *testing.T, typ string, scope map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": typ, "name": "throttle",
		"data": map[string]any{"scope": scope, "rate_limit": map[string]any{"requests_per_min": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestThrottleScopeShapeIsValidated(t *testing.T) {
	ids := make([]any, maxThrottleScopeIDs+1)
	for i := range ids {
		ids[i] = i + 1
	}
	cases := []struct {
		name  string
		typ   string
		scope map[string]any
		want  string
	}{
		{"agent ids without a project", "rate_limit", map[string]any{"agent_ids": []any{3}}, "exactly one entry"},
		{"agent ids with two projects", "rate_limit", map[string]any{"project_ids": []any{1, 2}, "agent_ids": []any{3}}, "exactly one entry"},
		{"a fractional id", "rate_limit", map[string]any{"user_ids": []any{1.5}}, "positive whole-number"},
		{"a negative id", "rate_limit", map[string]any{"user_ids": []any{-1}}, "positive whole-number"},
		{"not a list", "rate_limit", map[string]any{"user_ids": 4}, "must be a list"},
		{"too many ids", "rate_limit", map[string]any{"user_ids": ids}, "the limit is 100"},
		{"on a budget", "budget", map[string]any{"user_ids": []any{4}}, "rate_limit entries only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &throttleQuerier{users: 1, agents: 1}
			rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance", throttleBody(t, tc.typ, tc.scope))
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("status %d body %s, want 400 naming %q", rr.Code, rr.Body.String(), tc.want)
			}
			if len(q.countSQL) != 0 {
				t.Fatal("a malformed scope reached the database")
			}
		})
	}
}

func TestThrottleIDsMustExist(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	saved := okRow("1", "rate_limit", "throttle", map[string]any{}, true)
	saved.vals[6], saved.vals[7] = &now, &now

	t.Run("an unknown user", func(t *testing.T) {
		q := &throttleQuerier{users: 1}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			throttleBody(t, "rate_limit", map[string]any{"user_ids": []any{4, 5}}))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "user that does not exist") {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("an unknown agent", func(t *testing.T) {
		q := &throttleQuerier{agents: 0}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			throttleBody(t, "rate_limit", map[string]any{"project_ids": []any{7}, "agent_ids": []any{3}}))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "agent that does not exist") {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(strings.Join(q.countSQL, " "), `"p_7".applications`) &&
			!strings.Contains(strings.Join(q.countSQL, " "), `p_7.applications`) {
			t.Fatalf("the agents were not looked up in project 7's schema: %v", q.countSQL)
		}
	})
	t.Run("a project with no schema", func(t *testing.T) {
		q := &throttleQuerier{agentErr: &pgconn.PgError{Code: "42P01"}}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			throttleBody(t, "rate_limit", map[string]any{"project_ids": []any{7}, "agent_ids": []any{3}}))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("ids that exist are saved", func(t *testing.T) {
		q := &throttleQuerier{users: 2, agents: 1}
		q.rowResult = saved
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			throttleBody(t, "rate_limit", map[string]any{"project_ids": []any{7}, "agent_ids": []any{3}, "user_ids": []any{4, 5, 4}}))
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
		if len(q.countSQL) != 2 {
			t.Fatalf("existence checks = %d, want 2", len(q.countSQL))
		}
	})
	t.Run("an update is checked too", func(t *testing.T) {
		q := &throttleQuerier{users: 0}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPut, "/governance/1",
			throttleBody(t, "rate_limit", map[string]any{"user_ids": []any{4}}))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
	})
}

// flatThrottleBody puts the throttle keys at the top level of data, where the
// gateway also reads them when data has no scope object.
func flatThrottleBody(t *testing.T, typ string, data map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": typ, "name": "throttle", "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The gateway reads agent_ids and user_ids from the top level of data when the
// scope object is absent or empty. Main must check them there too.
func TestTopLevelThrottleKeysAreValidated(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		data map[string]any
		want string
	}{
		{"top-level agent ids without a project", "rate_limit",
			map[string]any{"agent_ids": []any{3}}, "exactly one entry"},
		{"top-level agent ids beside an empty scope", "rate_limit",
			map[string]any{"scope": map[string]any{}, "agent_ids": []any{3}}, "exactly one entry"},
		{"a top-level fractional user id", "rate_limit",
			map[string]any{"user_ids": []any{1.5}}, "positive whole-number"},
		{"top-level user ids on a budget", "budget",
			map[string]any{"user_ids": []any{4}}, "rate_limit entries only"},
		{"top-level team ids", "rate_limit",
			map[string]any{"team_ids": []any{1}}, "team_ids is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &throttleQuerier{users: 1, agents: 1}
			rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance", flatThrottleBody(t, tc.typ, tc.data))
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("status %d body %s, want 400 naming %q", rr.Code, rr.Body.String(), tc.want)
			}
		})
	}

	t.Run("a top-level unknown user", func(t *testing.T) {
		q := &throttleQuerier{users: 0}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			flatThrottleBody(t, "rate_limit", map[string]any{"user_ids": []any{4}}))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "user that does not exist") {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("a top-level agent of the top-level project is looked up there", func(t *testing.T) {
		q := &throttleQuerier{agents: 0}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			flatThrottleBody(t, "rate_limit", map[string]any{"project_ids": []any{7}, "agent_ids": []any{3}}))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "agent that does not exist") {
			t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(strings.Join(q.countSQL, " "), "p_7") {
			t.Fatalf("the agents were not looked up in project 7's schema: %v", q.countSQL)
		}
	})
	t.Run("a non-empty scope wins over the top level, as in the gateway", func(t *testing.T) {
		// The gateway ignores top-level keys when scope has any key, so they
		// are not checked here either: they have no effect.
		q := &throttleQuerier{users: 1}
		rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance",
			flatThrottleBody(t, "rate_limit", map[string]any{
				"scope":     map[string]any{"user_ids": []any{4}},
				"agent_ids": []any{3},
			}))
		if rr.Code == http.StatusBadRequest && strings.Contains(rr.Body.String(), "agent_ids") {
			t.Fatalf("a top-level key the gateway ignores was refused: %s", rr.Body.String())
		}
	})
}

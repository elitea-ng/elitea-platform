package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// holdsOnly is a Holds that answers from a fixed permission set.
func holdsOnly(granted ...string) func(*http.Request, ...string) bool {
	return func(_ *http.Request, names ...string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return slices.Contains(granted, n) })
	}
}

func TestGovernanceWriterDecidesByTheKindOfTheRow(t *testing.T) {
	cases := []struct {
		holder string
		may    map[string]bool
	}{
		{adminaccess.ConfigurationGovernance, map[string]bool{
			"guardrail": true, "routing_rule": true, "rate_limit": true, "egress_allowlist": true, "unknown": true,
		}},
		{adminaccess.LLMPolicyManage, map[string]bool{
			"guardrail": false, "routing_rule": true, "rate_limit": true, "budget": true, "model_config": true,
			"egress_allowlist": false, "mcp_allowlist": false, "credential_policy": false, "budget_alert": false,
			"unknown": false,
		}},
		{adminaccess.LLMGuardrailsManage, map[string]bool{
			"guardrail": true, "routing_rule": false, "rate_limit": false, "egress_allowlist": false,
		}},
		{adminaccess.LLMPolicyView, map[string]bool{"guardrail": false, "routing_rule": false}},
	}
	for _, c := range cases {
		h := &GovernanceHandler{holds: holdsOnly(c.holder)}
		writer := h.writer(&http.Request{})
		for rowType, want := range c.may {
			if got := writer.may(rowType); got != want {
				t.Errorf("holder %s may write %q = %v, want %v", c.holder, rowType, got, want)
			}
		}
	}
	if (&GovernanceHandler{}).writer(&http.Request{}).may("routing_rule") {
		t.Error("a handler with no Holds may write: it must write nothing")
	}
}

// storedKindQuerier answers the write statements as Postgres would for one
// stored row of storedType: the write matches only when its kind clause
// admits the stored type, and the existence read finds the row.
type storedKindQuerier struct {
	storedType string
	wrote      bool
}

func (q *storedKindQuerier) admits(args []any, first int) bool {
	legacy, _ := args[first].(bool)
	guardrails, _ := args[first+2].(bool)
	policy, _ := args[first+4].(bool)
	types, _ := args[first+3].([]string)
	if tokenNoGuardrail, _ := args[first+5].(bool); tokenNoGuardrail && q.storedType == args[first+1] {
		return false
	}
	return legacy || (q.storedType == args[first+1] && guardrails) || (slices.Contains(types, q.storedType) && policy)
}

func (q *storedKindQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &fakeRows{}, nil
}

func (q *storedKindQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.HasPrefix(sql, "SELECT type FROM"):
		return &fakeRow{vals: []any{q.storedType}}
	case strings.Contains(sql, "INSERT INTO gateway.governance_config"):
		q.wrote = true
		return okRow("id1", args[0].(string), "n", map[string]any{}, true)
	case strings.Contains(sql, "UPDATE gateway.governance_config"):
		if !q.admits(args, 6) {
			return &fakeRow{scanErr: pgx.ErrNoRows}
		}
		q.wrote = true
		return okRow("id1", args[0].(string), "n", map[string]any{}, true)
	}
	return &fakeRow{scanErr: pgx.ErrNoRows}
}

func (q *storedKindQuerier) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	if !q.admits(args, 1) {
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	q.wrote = true
	return pgconn.NewCommandTag("DELETE 1"), nil
}

// A policy manager without llm.guardrails.manage is refused every write of a
// guardrail: creating one, turning a stored one into a throttle rule, and
// deleting one. A guardrails manager does all three.
func TestPolicyManagerCannotWriteAGuardrail(t *testing.T) {
	const throttleBody = `{"type":"rate_limit","name":"r","data":{}}`
	const guardrailBody = `{"type":"guardrail","name":"g","data":{"guardrail":{"stage":"input",` +
		`"detectors":[{"type":"secrets","action":"block"}]}}}`
	for _, c := range []struct {
		holder string
		want   int
	}{
		{adminaccess.LLMPolicyManage, http.StatusForbidden},
		{adminaccess.LLMGuardrailsManage, 0},
	} {
		gates := openGovernanceGates
		gates.Holds = holdsOnly(c.holder)

		create := &storedKindQuerier{}
		if got := doJSONWith(NewGovernanceHandler(create), gates, http.MethodPost, "/governance", guardrailBody).Code; c.want != 0 && got != c.want {
			t.Errorf("%s: create a guardrail = %d, want %d", c.holder, got, c.want)
		} else if c.want == 0 && got == http.StatusForbidden {
			t.Errorf("%s: create a guardrail = 403, want admitted", c.holder)
		}

		retype := &storedKindQuerier{storedType: guardrailType}
		got := doJSONWith(NewGovernanceHandler(retype), gates, http.MethodPut, "/governance/id1", throttleBody).Code
		switch {
		case c.holder == adminaccess.LLMPolicyManage && (got != http.StatusForbidden || retype.wrote):
			t.Errorf("%s: turn a guardrail into a throttle rule = %d (wrote %v), want 403 and no write", c.holder, got, retype.wrote)
		case c.holder == adminaccess.LLMGuardrailsManage && got != http.StatusForbidden:
			// The body is a throttle rule, which a guardrails manager may not
			// write either: the refusal comes from the body, before the row.
			t.Errorf("%s: write a throttle rule = %d, want 403", c.holder, got)
		}

		remove := &storedKindQuerier{storedType: guardrailType}
		got = doJSONWith(NewGovernanceHandler(remove), gates, http.MethodDelete, "/governance/id1", "").Code
		if c.holder == adminaccess.LLMPolicyManage && (got != http.StatusForbidden || remove.wrote) {
			t.Errorf("%s: delete a guardrail = %d (wrote %v), want 403 and no delete", c.holder, got, remove.wrote)
		}
		if c.holder == adminaccess.LLMGuardrailsManage && (got != http.StatusNoContent || !remove.wrote) {
			t.Errorf("%s: delete a guardrail = %d (wrote %v), want 204", c.holder, got, remove.wrote)
		}

		edit := &storedKindQuerier{storedType: guardrailType}
		got = doJSONWith(NewGovernanceHandler(edit), gates, http.MethodPut, "/governance/id1", guardrailBody).Code
		if c.holder == adminaccess.LLMGuardrailsManage && (got != http.StatusOK || !edit.wrote) {
			t.Errorf("%s: edit a guardrail = %d (wrote %v), want 200", c.holder, got, edit.wrote)
		}
	}
}

// A policy manager edits a stored throttle rule; a missing row is a 404, not a
// 403.
func TestPolicyManagerEditsAThrottleRule(t *testing.T) {
	gates := openGovernanceGates
	gates.Holds = holdsOnly(adminaccess.LLMPolicyManage)
	const body = `{"type":"rate_limit","name":"r","data":{}}`

	stored := &storedKindQuerier{storedType: "rate_limit"}
	if got := doJSONWith(NewGovernanceHandler(stored), gates, http.MethodPut, "/governance/id1", body).Code; got != http.StatusOK || !stored.wrote {
		t.Errorf("edit a throttle rule = %d (wrote %v), want 200", got, stored.wrote)
	}
	missing := &fakeQuerier{}
	if got := doJSONWith(NewGovernanceHandler(missing), gates, http.MethodPut, "/governance/id1", body).Code; got != http.StatusNotFound {
		t.Errorf("edit a missing row = %d, want 404", got)
	}
}

func TestGovernanceRegisterRefusesAMissingGate(t *testing.T) {
	for name, gates := range map[string]GovernanceGates{
		"view":   {Manage: passGate, Holds: openGovernanceGates.Holds},
		"manage": {View: passGate, Holds: openGovernanceGates.Holds},
		"holds":  {View: passGate, Manage: passGate},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register without %s did not panic", name)
				}
			}()
			NewGovernanceHandler(&fakeQuerier{}).Routes(gates)
		}()
	}
}

// doJSONWith is doJSON behind the given gates.
func doJSONWith(h *GovernanceHandler, gates GovernanceGates, method, target, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.Routes(gates).ServeHTTP(rr, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rr
}

// A delete that matched no row and whose follow-up read fails is a 500, not
// a 404: an outage must not read as a missing row.
func TestAMissedDeleteWithAFailedReadIs500(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: errors.New("pool exhausted")}, execTag: pgconn.NewCommandTag("DELETE 0")}
	if got := doJSONWith(NewGovernanceHandler(q), openGovernanceGates, http.MethodDelete, "/governance/id1", "").Code; got != http.StatusInternalServerError {
		t.Errorf("delete = %d, want 500", got)
	}
}

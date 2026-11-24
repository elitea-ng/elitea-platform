package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The token rule of the action table on the governance rows (ADR-0059 R1.2b,
// issue 2993): the middleware leaves the decision to the handler
// (adminaccess.TokenRuleInHandler), which refuses an inherit token the
// guardrail outcome only. The caller holds configuration.governance, so the
// kind check of the gate admits every row and only the token rule refuses.

const (
	tokenGuardrailBody = `{"type":"guardrail","name":"g","data":{"guardrail":{"stage":"input",` +
		`"detectors":[{"type":"secrets","action":"block"}]}}}`
	tokenThrottleBody = `{"type":"rate_limit","name":"r","data":{}}`
)

func doTokenJSON(h *GovernanceHandler, method, target, body string) *httptest.ResponseRecorder {
	gates := openGovernanceGates
	gates.Holds = holdsOnly(adminaccess.ConfigurationGovernance)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req = req.WithContext(adminaccess.ContextWithTokenCaller(req.Context(),
		adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true}))
	rr := httptest.NewRecorder()
	h.Routes(gates).ServeHTTP(rr, req)
	return rr
}

func tokenRefusalCode(rr *httptest.ResponseRecorder) string {
	if rr.Code != http.StatusForbidden {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body.Code
}

func TestAnInheritTokenIsRefusedTheGuardrailOutcomeOnly(t *testing.T) {
	for _, tc := range []struct {
		name, method, target, body, stored string
		refused                            bool
	}{
		{"create a guardrail", http.MethodPost, "/governance", tokenGuardrailBody, "", true},
		{"create a throttle rule", http.MethodPost, "/governance", tokenThrottleBody, "", false},
		{"turn a throttle rule into a guardrail", http.MethodPut, "/governance/id1", tokenGuardrailBody, "rate_limit", true},
		{"turn a guardrail into a throttle rule", http.MethodPut, "/governance/id1", tokenThrottleBody, guardrailType, true},
		{"edit a throttle rule", http.MethodPut, "/governance/id1", tokenThrottleBody, "rate_limit", false},
		{"delete a guardrail", http.MethodDelete, "/governance/id1", "", guardrailType, true},
		{"delete a throttle rule", http.MethodDelete, "/governance/id1", "", "rate_limit", false},
	} {
		q := &storedKindQuerier{storedType: tc.stored}
		rr := doTokenJSON(NewGovernanceHandler(q), tc.method, tc.target, tc.body)
		code := tokenRefusalCode(rr)
		if tc.refused {
			if code != adminaccess.CodeTokenCriticalAction || q.wrote {
				t.Errorf("%s = %d %q (wrote %v), want 403 %s and no write",
					tc.name, rr.Code, code, q.wrote, adminaccess.CodeTokenCriticalAction)
			}
			continue
		}
		if code != "" || rr.Code >= 400 {
			t.Errorf("%s = %d %q, want the write", tc.name, rr.Code, code)
		}
		if !q.wrote {
			t.Errorf("%s: the stored row was not written", tc.name)
		}
	}
}

// raceQuerier is a row that is a guardrail when the write's statement runs
// and a throttle rule by the time the miss is read back.
type raceQuerier struct{ storedKindQuerier }

func (q *raceQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := q.storedKindQuerier.Exec(ctx, sql, args...)
	q.storedType = "rate_limit"
	return tag, err
}

// A token's miss on a row that stopped being a guardrail is a 409 to retry,
// not a missing permission (review of #2997).
func TestATokenMissOnARowThatChangedIsAConflict(t *testing.T) {
	q := &raceQuerier{storedKindQuerier{storedType: guardrailType}}
	rr := doTokenJSON(NewGovernanceHandler(q), http.MethodDelete, "/governance/id1", "")
	if rr.Code != http.StatusConflict || q.wrote {
		t.Fatalf("delete = %d %s (wrote %v), want 409 and no write", rr.Code, rr.Body, q.wrote)
	}
}

// A browser session is not a token: the same caller writes a guardrail.
func TestASessionWritesAGuardrail(t *testing.T) {
	gates := openGovernanceGates
	gates.Holds = holdsOnly(adminaccess.ConfigurationGovernance)
	q := &storedKindQuerier{storedType: guardrailType}
	if got := doJSONWith(NewGovernanceHandler(q), gates, http.MethodDelete, "/governance/id1", "").Code; got != http.StatusNoContent || !q.wrote {
		t.Errorf("session delete of a guardrail = %d (wrote %v), want 204", got, q.wrote)
	}
}

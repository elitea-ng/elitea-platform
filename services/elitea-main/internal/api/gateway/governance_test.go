package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- fakes over the narrow governanceQuerier seam -------------------------

// fakeRow is a single-row Scan target. When scanErr is set, Scan returns it;
// otherwise it copies vals into the destinations positionally.
type fakeRow struct {
	vals    []any
	scanErr error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return assign(dest, r.vals)
}

// fakeRows iterates a fixed set of value slices.
type fakeRows struct {
	pgx.Rows // embedded so unused methods are satisfied; nil-panics if hit
	rows     [][]any
	idx      int
	closed   bool
}

func (f *fakeRows) Next() bool { return f.idx < len(f.rows) }
func (f *fakeRows) Close()     { f.closed = true }
func (f *fakeRows) Err() error { return nil }
func (f *fakeRows) Scan(dest ...any) error {
	err := assign(dest, f.rows[f.idx])
	f.idx++
	return err
}

// assign copies src values into the pointer destinations positionally.
func assign(dest []any, src []any) error {
	if len(dest) != len(src) {
		return errors.New("scan: column count mismatch")
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d, _ = src[i].(string)
		case *bool:
			*d, _ = src[i].(bool)
		case *[]byte:
			if b, ok := src[i].([]byte); ok {
				*d = b
			}
		case **time.Time:
			if t, ok := src[i].(*time.Time); ok {
				*d = t
			}
		default:
			return errors.New("scan: unsupported destination type")
		}
	}
	return nil
}

// fakeQuerier routes Query/QueryRow/Exec to preset results.
type fakeQuerier struct {
	queryRows *fakeRows
	queryErr  error
	rowResult *fakeRow
	execTag   pgconn.CommandTag
	execErr   error
	lastSQL   string
	lastArgs  []any
}

func (q *fakeQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.lastSQL, q.lastArgs = sql, args
	if q.queryErr != nil {
		return nil, q.queryErr
	}
	if q.queryRows == nil {
		return &fakeRows{}, nil
	}
	return q.queryRows, nil
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.lastSQL, q.lastArgs = sql, args
	if q.rowResult == nil {
		return &fakeRow{scanErr: pgx.ErrNoRows}
	}
	return q.rowResult
}

func (q *fakeQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.lastSQL, q.lastArgs = sql, args
	return q.execTag, q.execErr
}

// okRow builds a fakeRow matching the RETURNING/SELECT column order used by the
// handler: id, type, section, name, data, enabled, created_at, updated_at.
func okRow(id, typ, name string, data map[string]any, enabled bool) *fakeRow {
	b, _ := json.Marshal(data)
	now := time.Unix(0, 0).UTC()
	return &fakeRow{vals: []any{id, typ, "governance", name, b, enabled, &now, &now}}
}

func doJSON(h *GovernanceHandler, method, target string, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBuffer(nil)
	} else {
		rdr = bytes.NewBufferString(body)
	}
	h.Routes().ServeHTTP(rr, httptest.NewRequest(method, target, rdr))
	return rr
}

// --- CEL compilation ------------------------------------------------------

func TestCompileRoutingCEL(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"valid bool over vars", `provider == "openai" && budget_used < 0.9`, false},
		{"valid over customer and params", `customer_id == "acme" && params.stream == true`, false},
		// The four variables below TYPE-CHECK — they are declared in the same
		// environment as the rest — and are refused anyway, because the gateway
		// has no value to put in them. A rule naming one would report itself
		// valid and never match. See unevaluableCELVariables in routing_cel.go.
		{"refuses tokens_used", `tokens_used > 1000`, true},
		{"refuses headers and complexity_tier", `headers["x-tier"] == complexity_tier`, true},
		{"refuses team_id", `team_id == "7"`, true},
		// A map KEY that happens to spell a refused variable is not a reference
		// to it. This is the case the lexical check has to get right.
		{"params key named headers is not the headers variable", `params.headers == "x"`, false},
		{"identifier prefix is not a reference", `provider == "team_idle"`, false},
		{"empty", ``, true},
		{"non-bool result", `provider + model`, true},
		{"int result", `budget_used + 1.0`, true},
		{"syntax error", `provider ==`, true},
		{"unknown variable", `unknown_var == "x"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CompileRoutingCEL(tc.expr)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.expr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.expr, err)
			}
		})
	}
}

// --- routing-rule validation ---------------------------------------------

func TestValidateRoutingRule(t *testing.T) {
	valid := map[string]any{
		"cel": `provider == "openai"`,
		"targets": []any{
			map[string]any{"provider": "openai", "model": "gpt-4o", "weight": 0.7},
			map[string]any{"provider": "anthropic", "model": "claude", "weight": 0.3},
		},
	}
	if err := validateRoutingRule(valid); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	cases := []struct {
		name string
		data map[string]any
	}{
		{"bad cel", map[string]any{"cel": `provider ==`, "targets": []any{
			map[string]any{"provider": "p", "model": "m", "weight": 1.0}}}},
		{"no targets", map[string]any{"cel": `true`, "targets": []any{}}},
		{"weights over 1", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"provider": "p", "model": "m", "weight": 0.7},
			map[string]any{"provider": "q", "model": "n", "weight": 0.7}}}},
		{"weights under 1", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"provider": "p", "model": "m", "weight": 0.5}}}},
		{"missing provider", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"model": "m", "weight": 1.0}}}},
		{"missing model", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"provider": "p", "weight": 1.0}}}},
		{"non-numeric weight", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"provider": "p", "model": "m", "weight": "heavy"}}}},
		{"negative weight", map[string]any{"cel": `true`, "targets": []any{
			map[string]any{"provider": "p", "model": "m", "weight": -1.0},
			map[string]any{"provider": "q", "model": "n", "weight": 2.0}}}},
		{"target not object", map[string]any{"cel": `true`, "targets": []any{"nope"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRoutingRule(tc.data); err == nil {
				t.Errorf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestValidateGovernanceRow(t *testing.T) {
	if err := validateGovernanceRow(GovernanceRow{Type: "", Name: "x"}); err == nil {
		t.Error("empty type should be rejected")
	}
	if err := validateGovernanceRow(GovernanceRow{Type: "budget", Name: "  "}); err == nil {
		t.Error("blank name should be rejected")
	}
	if err := validateGovernanceRow(GovernanceRow{Type: "budget", Name: "monthly", Data: map[string]any{}}); err != nil {
		t.Errorf("plain budget row should pass: %v", err)
	}
	// routing_rule delegates to validateRoutingRule
	bad := GovernanceRow{Type: "routing_rule", Name: "r", Data: map[string]any{"cel": "provider =="}}
	if err := validateGovernanceRow(bad); err == nil {
		t.Error("routing rule with bad CEL should be rejected")
	}
}

// --- List -----------------------------------------------------------------

func TestListReturnsRows(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	data, _ := json.Marshal(map[string]any{"limit_usd": 10.0})
	q := &fakeQuerier{queryRows: &fakeRows{rows: [][]any{
		{"id1", "budget", "governance", "monthly", data, true, &now, &now},
	}}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodGet, "/governance", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Items []GovernanceRow `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", resp.Total)
	}
	if resp.Items[0].Type != "budget" || resp.Items[0].Name != "monthly" {
		t.Errorf("unexpected row: %+v", resp.Items[0])
	}
}

func TestListWithTypeFilter(t *testing.T) {
	q := &fakeQuerier{queryRows: &fakeRows{}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodGet, "/governance?type=routing_rule", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(q.lastArgs) != 1 || q.lastArgs[0] != "routing_rule" {
		t.Errorf("type filter not passed as arg: %v", q.lastArgs)
	}
}

func TestListQueryErrorReturnsEmpty(t *testing.T) {
	q := &fakeQuerier{queryErr: errors.New("db down")}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodGet, "/governance", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (graceful)", rr.Code)
	}
}

// --- Create ---------------------------------------------------------------

func TestCreateSuccess(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("newid", "budget", "monthly", map[string]any{"limit_usd": 5.0}, true)}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPost, "/governance",
		`{"type":"budget","name":"monthly","data":{"limit_usd":5.0},"enabled":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var row GovernanceRow
	_ = json.NewDecoder(rr.Body).Decode(&row)
	if row.ID != "newid" {
		t.Errorf("id = %q, want newid", row.ID)
	}
}

func TestCreateRoutingRuleValidatesServerSide(t *testing.T) {
	// Client sent weights that do not sum to 1.0 — server must reject regardless.
	q := &fakeQuerier{rowResult: okRow("x", "routing_rule", "r", nil, true)}
	h := NewGovernanceHandler(q)
	body := `{"type":"routing_rule","name":"r","data":{"cel":"provider == \"openai\"","targets":[{"provider":"openai","model":"gpt-4o","weight":0.5}]}}`
	rr := doJSON(h, http.MethodPost, "/governance", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (weights != 1.0)", rr.Code)
	}
}

func TestCreateValidRoutingRule(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("x", "routing_rule", "r", nil, true)}
	h := NewGovernanceHandler(q)
	body := `{"type":"routing_rule","name":"r","data":{"cel":"provider == \"openai\"","targets":[{"provider":"openai","model":"gpt-4o","weight":1.0}]}}`
	rr := doJSON(h, http.MethodPost, "/governance", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateMissingType(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPost, "/governance", `{"name":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCreateMalformedBody(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPost, "/governance", `{not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCreateUniqueViolation(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: &pgconn.PgError{Code: "23505"}}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPost, "/governance", `{"type":"budget","name":"dup","data":{}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestCreateInternalError(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: errors.New("boom")}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPost, "/governance", `{"type":"budget","name":"x","data":{}}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- Update ---------------------------------------------------------------

func TestUpdateSuccess(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("id9", "budget", "monthly", map[string]any{}, false)}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPut, "/governance/id9",
		`{"type":"budget","name":"monthly","data":{},"enabled":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUpdateNotFound(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: pgx.ErrNoRows}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPut, "/governance/missing", `{"type":"budget","name":"x","data":{}}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateValidationRejected(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPut, "/governance/id1", `{"type":"","name":"x","data":{}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestUpdateMalformedBody(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPut, "/governance/id1", `{oops`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestUpdateUniqueViolation(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: &pgconn.PgError{Code: "23505"}}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPut, "/governance/id1", `{"type":"budget","name":"dup","data":{}}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestUpdateInternalError(t *testing.T) {
	q := &fakeQuerier{rowResult: &fakeRow{scanErr: errors.New("boom")}}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodPut, "/governance/id1", `{"type":"budget","name":"x","data":{}}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- Delete ---------------------------------------------------------------

func TestDeleteSuccess(t *testing.T) {
	q := &fakeQuerier{execTag: pgconn.NewCommandTag("DELETE 1")}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodDelete, "/governance/id1", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestDeleteNotFound(t *testing.T) {
	q := &fakeQuerier{execTag: pgconn.NewCommandTag("DELETE 0")}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodDelete, "/governance/missing", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDeleteError(t *testing.T) {
	q := &fakeQuerier{execErr: errors.New("db down")}
	h := NewGovernanceHandler(q)
	rr := doJSON(h, http.MethodDelete, "/governance/id1", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// --- ValidateCEL action ---------------------------------------------------

func TestValidateCELActionValid(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPost, "/governance/validate-cel", `{"cel":"budget_used > 0.5"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["valid"] != true {
		t.Errorf("expected valid=true, got %v", resp)
	}
}

func TestValidateCELActionInvalid(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPost, "/governance/validate-cel", `{"cel":"provider =="}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid:false envelope)", rr.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["valid"] != false {
		t.Errorf("expected valid=false, got %v", resp)
	}
	if _, ok := resp["error"]; !ok {
		t.Error("expected an error message in the envelope")
	}
}

func TestValidateCELActionMalformed(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	rr := doJSON(h, http.MethodPost, "/governance/validate-cel", `{bad`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// --- Register / routing ---------------------------------------------------

func TestRegisterAttachesRoutes(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{queryRows: &fakeRows{}})
	r := chi.NewRouter()
	h.Register(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/governance", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("Register did not wire GET /governance: %d", rr.Code)
	}
}

// --- helpers --------------------------------------------------------------

func TestToFloat(t *testing.T) {
	if f, ok := toFloat(1.5); !ok || f != 1.5 {
		t.Errorf("float64 case: %v %v", f, ok)
	}
	if f, ok := toFloat(3); !ok || f != 3 {
		t.Errorf("int case: %v %v", f, ok)
	}
	if f, ok := toFloat(json.Number("2.5")); !ok || f != 2.5 {
		t.Errorf("json.Number case: %v %v", f, ok)
	}
	if _, ok := toFloat("x"); ok {
		t.Error("string should not coerce")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Error("PgError 23505 should be a unique violation")
	}
	if !isUniqueViolation(errors.New("duplicate key value violates unique constraint")) {
		t.Error("string fallback should detect unique violation")
	}
	if isUniqueViolation(errors.New("some other error")) {
		t.Error("unrelated error should not be a unique violation")
	}
}

// --- request-body and CEL bounds -------------------------------------------

// paddedJSON returns prefix + 'a'*k + suffix with k chosen so the result is
// exactly n bytes, so a test can sit on the byte limit and one past it.
func paddedJSON(t *testing.T, n int, prefix, suffix string) string {
	t.Helper()
	k := n - len(prefix) - len(suffix)
	if k < 0 {
		t.Fatalf("cannot pad to %d bytes: fixed part is %d", n, len(prefix)+len(suffix))
	}
	return prefix + strings.Repeat("a", k) + suffix
}

func TestGovernanceBodyBound(t *testing.T) {
	routes := []struct {
		name, method, target string
		prefix, suffix       string
	}{
		{"create", http.MethodPost, "/governance", `{"type":"budget","name":"n","data":{"pad":"`, `"}}`},
		{"update", http.MethodPut, "/governance/abc", `{"type":"budget","name":"n","data":{"pad":"`, `"}}`},
		{"validate-cel", http.MethodPost, "/governance/validate-cel", `{"cel":"provider == \"openai\"","pad":"`, `"}`},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/at limit", func(t *testing.T) {
			q := &fakeQuerier{rowResult: okRow("abc", "budget", "n", nil, true)}
			rr := doJSON(NewGovernanceHandler(q), rt.method, rt.target,
				paddedJSON(t, maxGovernanceRequestBytes, rt.prefix, rt.suffix))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
		})
		t.Run(rt.name+"/limit+1", func(t *testing.T) {
			q := &fakeQuerier{rowResult: okRow("abc", "budget", "n", nil, true)}
			rr := doJSON(NewGovernanceHandler(q), rt.method, rt.target,
				paddedJSON(t, maxGovernanceRequestBytes+1, rt.prefix, rt.suffix))
			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
			}
			var resp map[string]string
			_ = json.NewDecoder(rr.Body).Decode(&resp)
			if resp["error"] != "request body too large" {
				t.Errorf("error = %q, want %q", resp["error"], "request body too large")
			}
			if q.lastSQL != "" {
				t.Errorf("an oversized body reached the database: %q", q.lastSQL)
			}
		})
	}
}

// celOfLength returns a valid boolean routing predicate exactly n bytes long.
func celOfLength(t *testing.T, n int) string {
	t.Helper()
	return paddedJSON(t, n, `provider == "`, `"`)
}

func TestCompileRoutingCELLengthBound(t *testing.T) {
	if err := CompileRoutingCEL(celOfLength(t, maxRoutingCELBytes)); err != nil {
		t.Fatalf("expression at the limit refused: %v", err)
	}
	err := CompileRoutingCEL(celOfLength(t, maxRoutingCELBytes+1))
	if !errors.Is(err, ErrRoutingCELTooLong) {
		t.Fatalf("err = %v, want ErrRoutingCELTooLong", err)
	}
}

func TestValidateCELActionOverCELCap(t *testing.T) {
	h := NewGovernanceHandler(&fakeQuerier{})
	cel := celOfLength(t, maxRoutingCELBytes+1)
	body, _ := json.Marshal(ValidateCELRequest{CEL: cel})
	rr := doJSON(h, http.MethodPost, "/governance/validate-cel", string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid:false envelope)", rr.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["valid"] != false {
		t.Fatalf("expected valid=false, got %v", resp["valid"])
	}
	if msg, _ := resp["error"].(string); strings.Contains(msg, cel) {
		t.Error("the refusal echoes the expression back")
	}
}

func TestCreateRoutingRuleOverCELCap(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("x", "routing_rule", "r", nil, true)}
	data := map[string]any{
		"cel":     celOfLength(t, maxRoutingCELBytes+1),
		"targets": []any{map[string]any{"provider": "openai", "model": "gpt-4o", "weight": 1.0}},
	}
	body, _ := json.Marshal(GovernanceRow{Type: "routing_rule", Name: "r", Data: data})
	rr := doJSON(NewGovernanceHandler(q), http.MethodPost, "/governance", string(body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if q.lastSQL != "" {
		t.Errorf("a refused rule reached the database: %q", q.lastSQL)
	}
}

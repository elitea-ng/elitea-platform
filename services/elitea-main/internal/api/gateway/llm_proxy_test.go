package gateway

// llm_proxy_test.go — the admin LLM Proxy surface.
//
// The assertions here are weighted towards the failures that would be SILENT in
// production: a price override that the sync would revert, an override that
// prices nothing the cost path reads, a negative price that makes budgets
// recede, and an unreachable gateway reported as though it were reachable. Each
// of those produces a success response and a plausible screen, so no amount of
// manual clicking would find them.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// stubQuerier satisfies llmProxyQuerier without a database.
//
// It is deliberately inert: every write test in this file asserts a REFUSAL, and
// a refusal that is only produced because the fake database failed would pass
// against a handler that validated nothing. Panicking on use makes "the request
// reached the database" a test failure rather than a silent pass.
type stubQuerier struct{}

func (stubQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("the handler reached the database on a request that should have been refused")
}

func (stubQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("the handler reached the database on a request that should have been refused")
}

func (stubQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("the handler reached the database on a request that should have been refused")
}

// stubStatus is a StatusReader that answers with whatever it is given.
type stubStatus struct {
	body json.RawMessage
	err  error
}

func (s stubStatus) Status(context.Context) (json.RawMessage, error) { return s.body, s.err }

func doLLMProxy(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decodeLLMProxyBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestStatusReportsUnreachableAsAResult is the central claim of the status
// route: a gateway that cannot be answered for is a RESULT, not a failure.
//
// Returning 5xx here would be worse than useless. The admin screen cannot tell
// a 502 produced by the gateway being down from a 502 produced by its own
// request failing, so the one piece of information the operator came for —
// "the enforcement hop is down" — would be indistinguishable from noise.
func TestStatusReportsUnreachableAsAResult(t *testing.T) {
	h := NewLLMProxyHandler(nil, stubStatus{err: errors.New("dial tcp: connection refused")})

	rec := doLLMProxy(t, h.Routes(), http.MethodGet, "/status", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unreachable gateway is a result, not a request failure", rec.Code)
	}
	body := decodeLLMProxyBody(t, rec)
	if body["reachable"] != false {
		t.Errorf("reachable = %v, want false", body["reachable"])
	}
	// The transport's own sentence, not a generic one: "not configured" and
	// "connection refused" call for different actions.
	if msg, _ := body["error"].(string); !strings.Contains(msg, "connection refused") {
		t.Errorf("error = %q, want the transport's own reason", msg)
	}
}

// TestStatusReportsNotConfiguredDistinctly pins the difference between a
// deployment that never wired the gateway and one whose gateway is down. Both
// report reachable:false, and collapsing their explanations would send an
// operator to debug a network path that was never meant to exist.
func TestStatusReportsNotConfiguredDistinctly(t *testing.T) {
	h := NewLLMProxyHandler(nil, nil)

	rec := doLLMProxy(t, h.Routes(), http.MethodGet, "/status", "")

	body := decodeLLMProxyBody(t, rec)
	if body["reachable"] != false {
		t.Fatalf("reachable = %v, want false", body["reachable"])
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "no LLM gateway address configured") {
		t.Errorf("error = %q, want it to name the missing configuration", msg)
	}
}

// TestStatusPassesTheGatewayBodyThrough pins that this route does not reshape
// what the gateway said. Re-declaring the body here would be a second
// specification of a contract the gateway owns — free to drift silently, which
// is the exact failure the status route exists to expose.
func TestStatusPassesTheGatewayBodyThrough(t *testing.T) {
	upstream := `{"enabled":true,"rate_limits_enforceable":false,"workload_identity_enabled":true,"definitions":{"rows":4,"rejected":[{"id":"a","type":"routing_rule","name":"r","reason":"CEL compile error"}]}}`
	h := NewLLMProxyHandler(nil, stubStatus{body: json.RawMessage(upstream)})

	rec := doLLMProxy(t, h.Routes(), http.MethodGet, "/status", "")

	body := decodeLLMProxyBody(t, rec)
	if body["reachable"] != true {
		t.Fatalf("reachable = %v, want true", body["reachable"])
	}
	gw, ok := body["gateway"].(map[string]any)
	if !ok {
		t.Fatalf("gateway missing from %s", rec.Body.String())
	}
	// The fields an operator acts on must survive the hop verbatim.
	if gw["rate_limits_enforceable"] != false {
		t.Errorf("rate_limits_enforceable did not survive: %v", gw["rate_limits_enforceable"])
	}
	// The Admin UI offers the keyless auth mode only when the gateway armed it.
	if gw["workload_identity_enabled"] != true {
		t.Errorf("workload_identity_enabled did not survive: %v", gw["workload_identity_enabled"])
	}
	defs, _ := gw["definitions"].(map[string]any)
	rejected, _ := defs["rejected"].([]any)
	if len(rejected) != 1 {
		t.Fatalf("rejected rows did not survive: %v", defs["rejected"])
	}
}

// TestGatewayStatusClientRejectsANonJSONBody pins that the client validates
// what it read. Passing an arbitrary body through would let a proxy error page
// render on the admin screen as though it were the gateway's own report — which
// reads as "the gateway answered", the single conclusion this surface must
// never produce falsely.
func TestGatewayStatusClientRejectsANonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	_, err := NewGatewayStatusClient(srv.URL, nil).Status(context.Background())

	if err == nil {
		t.Fatal("a non-JSON body was accepted as a gateway status report")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("error = %q, want it to name the cause", err)
	}
}

// TestGatewayStatusClientRequestsTheRightPath pins the upstream route. A typo
// here would surface as an unreachable gateway on a healthy deployment.
func TestGatewayStatusClientRequestsTheRightPath(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		// No signature is expected on this hop: the gateway's operator listener
		// verifies mutual TLS and no HMAC. Sending one would imply an
		// authorization the upstream does not perform.
		if r.Header.Get("X-Elitea-Identity-Signature") != "" {
			t.Error("the status client signed a request the gateway does not verify")
		}
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer srv.Close()

	if _, err := NewGatewayStatusClient(srv.URL, nil).Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != "/governance/status" {
		t.Errorf("path = %q, want /governance/status", got)
	}
}

// TestUpsertModelRefusesAnOverrideWithNoPrice pins the worst outcome this
// surface can produce.
//
// Such a write marks the row `price_overridden`, which permanently excludes it
// from the price sync, while pricing nothing. The model then keeps billing at
// the rate `internal/cost` invents for an uncatalogued model — a prefix-table
// guess, or a flat 1.0/3.0 USD per 1M — and the sync that would have replaced
// that guess with a real price is precisely what the flag now prevents. It is
// one empty form away, and every layer below would report success.
func TestUpsertModelRefusesAnOverrideWithNoPrice(t *testing.T) {
	h := NewLLMProxyHandler(stubQuerier{}, nil)

	rec := doLLMProxy(t, h.Routes(), http.MethodPut, "/models",
		`{"provider":"openai","model_name":"gpt-5"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: an override that prices nothing pins the model to the invented fallback rate forever", rec.Code)
	}
}

// TestWritePathNamesOnlyColumnsTheCostPathReads pins the narrowing that makes
// the "at least one price" rule mean anything.
//
// `internal/cost`'s catalogue statement selects exactly six columns. The table
// holds three more — the two cache costs and the above-128k rate — that the
// price sync writes and NOTHING reads. While those were on this surface, an
// operator could satisfy hasAnyPrice with one of them: the row would be pinned
// off the sync, its token columns still NULL, and every call would keep billing
// at the invented fallback while the form reported success.
//
// The upsert must also never NAME those columns, so a value the sync wrote into
// one survives an override rather than being nulled by an update that does not
// mean to touch it.
func TestWritePathNamesOnlyColumnsTheCostPathReads(t *testing.T) {
	unread := []string{
		"cache_creation_input_token_cost",
		"cache_read_input_token_cost",
		"input_cost_per_1m_tokens_above_128k",
	}
	for _, col := range unread {
		if strings.Contains(upsertModelSQL, col) {
			t.Errorf("the upsert names %q, which the gateway's cost path never reads: "+
				"an override could satisfy the price rule with it and still bill the "+
				"invented fallback rate, and the write would null whatever the sync stored", col)
		}
		if strings.Contains(listModelsSQL, col) {
			t.Errorf("the catalogue read selects %q, which nothing reads and the editor "+
				"cannot write — a column shown but not actionable", col)
		}
	}
	// The six that ARE read must all be present in both halves.
	for _, col := range []string{
		"input_cost_per_1m_tokens", "output_cost_per_1m_tokens",
		"input_cost_per_1m_seconds", "output_cost_per_1m_seconds",
		"input_cost_per_1m_characters", "output_cost_per_1m_characters",
	} {
		if !strings.Contains(upsertModelSQL, col) {
			t.Errorf("the upsert cannot write %q, a column the cost path reads", col)
		}
	}
}

// TestUpsertModelRefusesANegativePrice pins the second silent-corruption path.
// A negative price is multiplied out by the cost calculator into NEGATIVE
// spend, which SUBTRACTS from a project's accumulated cost — so the more the
// model is called, the further every budget ceiling recedes. Nothing errors.
func TestUpsertModelRefusesANegativePrice(t *testing.T) {
	h := NewLLMProxyHandler(stubQuerier{}, nil)

	rec := doLLMProxy(t, h.Routes(), http.MethodPut, "/models",
		`{"provider":"openai","model_name":"gpt-5","input_cost_per_1m_tokens":-1}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a negative price makes budgets recede as usage grows", rec.Code)
	}
}

// TestUpsertModelRefusesAnUnknownField pins DisallowUnknownFields. A misspelled
// price key would otherwise be dropped in silence and the row stored with that
// dimension null — a model priced at zero for one of its dimensions, saved with
// a 200 and a success toast.
func TestUpsertModelRefusesAnUnknownField(t *testing.T) {
	h := NewLLMProxyHandler(stubQuerier{}, nil)

	rec := doLLMProxy(t, h.Routes(), http.MethodPut, "/models",
		`{"provider":"openai","model_name":"gpt-5","input_cost_per_1k_tokens":3}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a misspelled price key must not be silently dropped", rec.Code)
	}
}

// TestUpsertModelRequiresIdentity pins that a price cannot be authored without
// saying which model it prices. The unique key is (provider, model_name), so a
// blank one would collide with every other blank one.
func TestUpsertModelRequiresIdentity(t *testing.T) {
	h := NewLLMProxyHandler(stubQuerier{}, nil)

	for _, body := range []string{
		`{"provider":"","model_name":"gpt-5","input_cost_per_1m_tokens":3}`,
		`{"provider":"openai","model_name":"  ","input_cost_per_1m_tokens":3}`,
	} {
		rec := doLLMProxy(t, h.Routes(), http.MethodPut, "/models", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestUpsertModelMarksTheRowOverridden pins the whole point of the write path.
// A statement that stored the prices without setting `price_overridden` would
// work perfectly until the next sync tick and then revert, with a success
// response at the time of the edit.
func TestUpsertModelMarksTheRowOverridden(t *testing.T) {
	if !strings.Contains(upsertModelSQL, "price_overridden") {
		t.Fatal("the upsert does not set price_overridden: the authored price would be reverted by the next sync")
	}
	idx := strings.Index(upsertModelSQL, "ON CONFLICT")
	if idx < 0 {
		t.Fatal("the upsert has no ON CONFLICT clause, so re-pricing an existing model would fail on the unique key")
	}
	if !strings.Contains(upsertModelSQL[idx:], "price_overridden                    = true") {
		t.Error("the DO UPDATE half does not set price_overridden: re-pricing a synced model would leave it synced")
	}
	// `source` records which UPSTREAM priced the row. Overwriting it would
	// destroy the provenance of the number being replaced.
	if strings.Contains(upsertModelSQL, "source =") || strings.Contains(upsertModelSQL, "source  ") {
		t.Error("the upsert writes `source`, destroying the provenance of the price it replaces")
	}
}

// TestClearOverrideDoesNotDeleteTheRow pins the behaviour the word "revert"
// promises. Deleting instead would take effect immediately and bill every call
// to that model at ZERO until the next sync tick — turning a revert click into
// an unpriced window.
func TestClearOverrideDoesNotDeleteTheRow(t *testing.T) {
	if strings.Contains(strings.ToUpper(clearOverrideSQL), "DELETE") {
		t.Fatal("clearing an override deletes the row: the model would bill zero until the next sync tick")
	}
	if !strings.Contains(clearOverrideSQL, "price_overridden = false") {
		t.Error("clearing an override does not clear the flag, so the sync would keep skipping the row")
	}
	// Guarding on the flag is what makes a no-op reportable as 404 rather than
	// as a success that changed nothing.
	if !strings.Contains(clearOverrideSQL, "AND price_overridden") {
		t.Error("the update is not guarded on price_overridden, so a click on a synced row reports success")
	}
}

// TestListModelsCountsUncalledModelsAsZero pins the COALESCEs. A catalogue
// entry nobody called must report 0 usage, not vanish: the scan targets are not
// pointers, so a NULL arriving there is a scan error that would drop the row —
// losing exactly the entries an operator auditing an unused-but-priced model
// came to see.
func TestListModelsCountsUncalledModelsAsZero(t *testing.T) {
	if !strings.Contains(listModelsSQL, "LEFT JOIN") {
		t.Fatal("the catalogue read is not a LEFT JOIN: a model nobody called would disappear from the list")
	}
	for _, col := range []string{"COALESCE(u.requests, 0)", "COALESCE(u.total_tokens, 0)", "COALESCE(u.cost_usd, 0)"} {
		if !strings.Contains(listModelsSQL, col) {
			t.Errorf("%s missing: a NULL there is a scan error that silently drops the row", col)
		}
	}
	// Aggregating BEFORE the join keeps one row per catalogue entry. Joining
	// the raw events would multiply each catalogue row by its event count.
	if !strings.Contains(listModelsSQL, "GROUP BY provider, model") {
		t.Error("usage is not pre-aggregated, so each catalogue row would be duplicated per usage event")
	}
}

// TestUnpricedQueryFindsCalledButUncataloguedModels pins the finding this whole
// surface exists to surface: a model that was CALLED and has no price row is
// billed at a rate nobody chose — an invented fallback for a token model, or
// nothing at all for audio, which no ceiling can then stop.
func TestUnpricedQueryFindsCalledButUncataloguedModels(t *testing.T) {
	if !strings.Contains(unpricedModelsSQL, "NOT EXISTS") {
		t.Fatal("the unpriced report does not look for absent catalogue rows")
	}
	if !strings.Contains(unpricedModelsSQL, "gateway.llm_usage_events") {
		t.Error("the unpriced report does not read the usage table, so it cannot find a model that was called")
	}
}

// TestResolveWindowFallsBackRatherThanRefusing pins that a mistyped ?window=
// does not withhold the catalogue. The window is a display choice; refusing the
// pricing data over a reporting preference would hide the data that matters for
// the sake of the one that does not.
func TestResolveWindowFallsBackRatherThanRefusing(t *testing.T) {
	for _, in := range []string{"", "nonsense", "1y"} {
		name, d := resolveWindow(in)
		if name != defaultUsageWindow || d == 0 {
			t.Errorf("resolveWindow(%q) = (%q, %v), want the default window", in, name, d)
		}
	}
	if name, _ := resolveWindow("7d"); name != "7d" {
		t.Errorf("resolveWindow(%q) did not honour a known window", "7d")
	}
}

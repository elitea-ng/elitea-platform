package gateway

// metered_prices_test.go — the admin metered-prices surface (#1238).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/jackc/pgx/v5"
)

// recordingRow answers the upsert's RETURNING with the price it was given.
type recordingRow struct{ price string }

func (r recordingRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.price
	return nil
}

// recordingQuerier records the one write it receives; reads still panic.
type recordingQuerier struct {
	stubQuerier
	sql  string
	args []any
}

func (q *recordingQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql, q.args = sql, args
	return recordingRow{price: "0.00800000"}
}

// TestUpsertMeteredPriceMarksOverriddenAndNamesTheAuthor pins the override
// semantics and the audit of the write: the row is marked overridden on both
// halves of the upsert, the author is recorded, and the price reaches
// NUMERIC as the operator wrote it rather than through a float64.
func TestUpsertMeteredPriceMarksOverriddenAndNamesTheAuthor(t *testing.T) {
	db := &recordingQuerier{}
	h := NewLLMProxyHandler(db, nil)

	r := httptest.NewRequest(http.MethodPut, "/metered-prices",
		strings.NewReader(`{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":0.008}`))
	r = r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "7", Email: "ops@example.com"}))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(db.args) != 5 || db.args[0] != "tavily" || db.args[1] != "web_search" ||
		db.args[2] != "credit" || db.args[3] != "0.008" {
		t.Fatalf("upsert args = %#v", db.args)
	}
	if author, ok := db.args[4].(*int64); !ok || author == nil || *author != 7 {
		t.Fatalf("author = %#v, want user id 7", db.args[4])
	}
	conflict := strings.Index(db.sql, "ON CONFLICT (provider, operation)")
	if conflict < 0 {
		t.Fatal("the upsert is not keyed on (provider, operation)")
	}
	if !strings.Contains(db.sql[:conflict], "true, now(), $5") ||
		!strings.Contains(db.sql[conflict:], "price_overridden    = true") ||
		!strings.Contains(db.sql[conflict:], "price_overridden_by = EXCLUDED.price_overridden_by") {
		t.Error("the upsert does not mark the row overridden by its author on both halves")
	}
	if strings.Contains(db.sql[conflict:], "source") {
		t.Error("the DO UPDATE half rewrites source, losing the seeded price's provenance")
	}
	body := decodeLLMProxyBody(t, rec)
	if body["price_overridden"] != true || body["price_overridden_by"] != "7" || body["price_per_unit_usd"] != "0.008" {
		t.Fatalf("response = %v", body)
	}
}

// TestUpsertMeteredPriceAcceptsAnExplicitZero pins "no silent zero" from the
// other side: free is an authored 0, not an absent price.
func TestUpsertMeteredPriceAcceptsAnExplicitZero(t *testing.T) {
	db := &recordingQuerier{}
	rec := doLLMProxy(t, NewLLMProxyHandler(db, nil).Routes(), http.MethodPut, "/metered-prices",
		`{"provider":"ddg_instant","operation":"web_search","unit":"request","price_per_unit_usd":0}`)
	if rec.Code != http.StatusOK || db.args[3] != "0" {
		t.Fatalf("status = %d, args %#v", rec.Code, db.args)
	}
}

func TestUpsertMeteredPriceRefusals(t *testing.T) {
	h := NewLLMProxyHandler(stubQuerier{}, nil)
	for name, body := range map[string]string{
		"no price":          `{"provider":"tavily","operation":"web_search","unit":"credit"}`,
		"negative":          `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":-0.01}`,
		"slipped decimal":   `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":8000}`,
		"beyond scale":      `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":0.000000001}`,
		"exponent":          `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":8e-3}`,
		"unknown provider":  `{"provider":"google","operation":"web_search","unit":"credit","price_per_unit_usd":0.01}`,
		"unknown operation": `{"provider":"tavily","operation":"web_scan","unit":"credit","price_per_unit_usd":0.01}`,
		"unknown unit":      `{"provider":"tavily","operation":"web_search","unit":"token","price_per_unit_usd":0.01}`,
		"unknown field":     `{"provider":"tavily","operation":"web_search","unit":"credit","price_usd":0.01}`,
		"string exponent":   `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":"5e-7"}`,
		"string negative":   `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":"-0.01"}`,
		"string beyond":     `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":"0.000000001"}`,
		"string garbage":    `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":"cheap"}`,
		"null price":        `{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":null}`,
	} {
		rec := doLLMProxy(t, h.Routes(), http.MethodPut, "/metered-prices", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestMeteredPricesWithoutAPoolAre503(t *testing.T) {
	h := NewLLMProxyHandler(nil, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := doLLMProxy(t, h.Routes(), method, "/metered-prices", `{}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", method, rec.Code)
		}
	}
}

// TestUpsertMeteredPriceTakesADecimalString pins the web's wire: a price below
// 1e-6 is a decimal string ("0.0000005"), which a JS number would serialise as
// "5e-7". It reaches NUMERIC as written, and the answer is a decimal string.
func TestUpsertMeteredPriceTakesADecimalString(t *testing.T) {
	db := &recordingQuerier{}
	rec := doLLMProxy(t, NewLLMProxyHandler(db, nil).Routes(), http.MethodPut, "/metered-prices",
		`{"provider":"tavily","operation":"web_search","unit":"credit","price_per_unit_usd":"0.0000005"}`)
	if rec.Code != http.StatusOK || db.args[3] != "0.0000005" {
		t.Fatalf("status = %d, args %#v, body %s", rec.Code, db.args, rec.Body.String())
	}
	if body := decodeLLMProxyBody(t, rec); body["price_per_unit_usd"] != "0.008" {
		t.Fatalf("response price = %#v, want the stored decimal string", body["price_per_unit_usd"])
	}
}

func TestTrimDecimal(t *testing.T) {
	for in, want := range map[string]string{
		"0.00800000": "0.008", "0.00000000": "0", "1.00000000": "1", "0.00000050": "0.0000005", "12": "12",
	} {
		if got := trimDecimal(in); got != want {
			t.Errorf("trimDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}

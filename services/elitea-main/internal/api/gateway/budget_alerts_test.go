package gateway

// Unit coverage for the budget-alerts surface: request validation, and the
// refusal to answer at all without a database pool.
//
// The persistence itself is covered by budget_alerts_postgres_integration_test.go,
// which is the only place it CAN be covered: issue #322 is precisely a store
// that satisfied every in-process assertion and persisted nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStoreWithoutPoolReportsAnError pins the wiring fault as an error rather
// than a silent in-memory fallback. An in-memory fallback is the defect this
// package was built around, and from the outside it is indistinguishable from a
// working surface — which is why it survived until #322.
func TestStoreWithoutPoolReportsAnError(t *testing.T) {
	store := NewBudgetAlertStore(nil)

	if _, err := store.Get(t.Context()); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Get error = %v, want ErrNoPool", err)
	}
	enabled := false
	if _, err := store.Update(t.Context(), BudgetAlertUpdateRequest{Enabled: &enabled}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Update error = %v, want ErrNoPool", err)
	}
}

// TestHandlerReportsAStoreFailureAsAnError is the whole point of #322: the
// surface must not report success for a policy it did not store.
func TestHandlerReportsAStoreFailureAsAnError(t *testing.T) {
	h := NewBudgetAlertHandler(NewBudgetAlertStore(nil))

	for _, tc := range []struct {
		name   string
		method string
		body   string
	}{
		{name: "read", method: http.MethodGet, body: ""},
		{name: "write", method: http.MethodPut, body: `{"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/budget-alerts", bytes.NewBufferString(tc.body))
			h.Routes(passGate, passGate).ServeHTTP(rr, req)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 — a surface that cannot persist must not answer 200", rr.Code)
			}
		})
	}
}

func TestUpdateRejectsAThresholdOutsideOneToOneHundred(t *testing.T) {
	h := NewBudgetAlertHandler(NewBudgetAlertStore(nil))

	for _, body := range []string{
		`{"threshold_pct":0}`,
		`{"threshold_pct":101}`,
		`{"threshold_pct":-5}`,
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/budget-alerts", bytes.NewBufferString(body))
		h.Routes(passGate, passGate).ServeHTTP(rr, req)
		// 400 rather than 500: validation runs before the store is consulted,
		// so a nil pool cannot mask a missing bounds check.
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, rr.Code)
		}
	}
}

func TestUpdateRejectsAMalformedBody(t *testing.T) {
	h := NewBudgetAlertHandler(NewBudgetAlertStore(nil))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/budget-alerts", bytes.NewBufferString(`{not json`))
	h.Routes(passGate, passGate).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestStoredConfigResolvesTheShippedDefaults covers the row shapes a database
// can hold: absent, partial, and complete. A partial row must not resolve the
// missing key to its zero — `enabled:false, threshold_pct:0` is a working
// config that silences every alert and would look deliberate.
func TestStoredConfigResolvesTheShippedDefaults(t *testing.T) {
	enabled := false
	pct := 55

	for _, tc := range []struct {
		name   string
		stored storedConfig
		want   BudgetAlertConfig
		notes  string
	}{
		{
			name:   "empty row",
			stored: storedConfig{},
			want:   BudgetAlertConfig{Enabled: true, ThresholdPct: DefaultSoftAlertThresholdPct},
		},
		{
			name:   "only enabled",
			stored: storedConfig{Enabled: &enabled},
			want:   BudgetAlertConfig{Enabled: false, ThresholdPct: DefaultSoftAlertThresholdPct},
		},
		{
			name:   "only threshold",
			stored: storedConfig{ThresholdPct: &pct},
			want:   BudgetAlertConfig{Enabled: true, ThresholdPct: 55},
		},
		{
			name:   "both",
			stored: storedConfig{Enabled: &enabled, ThresholdPct: &pct},
			want:   BudgetAlertConfig{Enabled: false, ThresholdPct: 55},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stored.resolve(); got != tc.want {
				t.Fatalf("resolve() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUpdatePatchOmitsUntouchedKeys pins the shape sent to the jsonb merge. A
// patch that spelled out both keys would overwrite the field the caller did not
// mention, which is what "partial update" must not do.
func TestUpdatePatchOmitsUntouchedKeys(t *testing.T) {
	enabled := false
	encoded, err := json.Marshal(storedConfig{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); got != `{"enabled":false}` {
		t.Fatalf("patch = %s, want only the key the caller supplied", got)
	}
}

package budgets

// The parts of this package that hold no database: the reporting period, the
// exact-decimal marshalling, and the write payload's validation. They run in
// CI, where the PostgreSQL integration tests skip.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The period MUST match the gateway's billingPeriodStart exactly. The
// accumulator's unique key is (scope, scope_id, period_start), so a period an
// hour or a timezone away from the gateway's looks up a row that does not exist
// and reports zero spend against a real limit — a budget that silently stops
// mattering rather than one that visibly breaks.
func TestPeriodMatchesTheGatewaysMonthBoundary(t *testing.T) {
	cases := []struct {
		name       string
		now        time.Time
		start, end time.Time
		label      string
		first      string
		last       string
	}{
		{
			name:  "mid-month",
			now:   time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC),
			start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			label: "202608", first: "2026-08-01", last: "2026-08-31",
		},
		{
			name:  "December rolls into the next year",
			now:   time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			start: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			label: "202612", first: "2026-12-01", last: "2026-12-31",
		},
		{
			name:  "February in a leap year keeps its 29th",
			now:   time.Date(2028, 2, 15, 0, 0, 0, 0, time.UTC),
			start: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
			label: "202802", first: "2028-02-01", last: "2028-02-29",
		},
		{
			// A local timestamp east of UTC on the 1st is still the previous
			// month in UTC — the case that makes "just use the local month"
			// wrong for one day out of every thirty.
			name:  "a non-UTC instant is normalised to UTC first",
			now:   time.Date(2026, 9, 1, 2, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)),
			start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			label: "202608", first: "2026-08-01", last: "2026-08-31",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			period := periodFor(testCase.now)
			if !period.start.Equal(testCase.start) {
				t.Fatalf("start = %s, want %s", period.start, testCase.start)
			}
			if !period.end.Equal(testCase.end) {
				t.Fatalf("end = %s, want %s", period.end, testCase.end)
			}
			if period.label() != testCase.label {
				t.Fatalf("label = %s, want %s", period.label(), testCase.label)
			}
			if period.firstDay() != testCase.first {
				t.Fatalf("firstDay = %s, want %s", period.firstDay(), testCase.first)
			}
			if period.lastDay() != testCase.last {
				t.Fatalf("lastDay = %s, want %s", period.lastDay(), testCase.last)
			}
			if period.resetsAt() != testCase.end.Format(time.RFC3339) {
				t.Fatalf("resetsAt = %s, want %s", period.resetsAt(), testCase.end.Format(time.RFC3339))
			}
		})
	}
}

// numeric must carry PostgreSQL's decimal through untouched. A value that went
// via float64 comes back as 0.10000000000000001 or loses its trailing zeros,
// and both are wrong on a money field.
func TestNumericPreservesTheExactDecimal(t *testing.T) {
	for _, want := range []string{"0.10", "100.00", "0", "1234567890.12345678"} {
		text := want
		encoded, err := json.Marshal(numeric(&text))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != want {
			t.Fatalf("numeric(%q) marshalled as %s", want, encoded)
		}
	}
	encoded, err := json.Marshal(numeric(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "null" {
		t.Fatalf("numeric(nil) marshalled as %s, want null", encoded)
	}
}

func TestDecodeBudgetWriteValidatesThePayload(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		check    func(*testing.T, parsedBudgetWrite)
	}{
		{
			name: "a complete payload", body: `{"monthly_limit": 12.50, "enabled": true, "currency": "USD", "soft_alert_pct": 90}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				// The ORIGINAL decimal text is what reaches PostgreSQL: the
				// float parse in the validator is a range check only.
				if parsed.monthlyLimit == nil || *parsed.monthlyLimit != "12.50" {
					t.Fatalf("monthlyLimit = %v, want the literal 12.50", parsed.monthlyLimit)
				}
				if !parsed.enabled || parsed.softAlertPct == nil || *parsed.softAlertPct != 90 {
					t.Fatalf("parsed = %+v", parsed)
				}
			},
		},
		{
			name: "an omitted limit means no ceiling", body: `{"enabled": true}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if parsed.monthlyLimit != nil {
					t.Fatalf("monthlyLimit = %v, want nil", *parsed.monthlyLimit)
				}
				// The reference defaults `enabled` to True on a write, which is
				// NOT the default it reports on a read of a missing row.
				if !parsed.enabled {
					t.Fatal("enabled defaulted to false on a write")
				}
			},
		},
		{
			name: "an explicit null limit means no ceiling", body: `{"monthly_limit": null}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if parsed.monthlyLimit != nil {
					t.Fatalf("monthlyLimit = %v, want nil", *parsed.monthlyLimit)
				}
			},
		},
		{
			name: "the two project policy fields", body: `{"budget_period": "monthly", "nats_fail_mode": "fail_closed"}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if parsed.budgetPeriod == nil || *parsed.budgetPeriod != "monthly" {
					t.Fatalf("budgetPeriod = %v, want monthly", parsed.budgetPeriod)
				}
				if !parsed.natsFailModeSet {
					t.Fatal("natsFailModeSet = false for a present field")
				}
				if parsed.natsFailMode == nil || *parsed.natsFailMode != "fail_closed" {
					t.Fatalf("natsFailMode = %v, want fail_closed", parsed.natsFailMode)
				}
				if !parsed.projectPolicyRequested() {
					t.Fatal("projectPolicyRequested() = false; the member PUT would accept this")
				}
			},
		},
		{
			// The distinction a *string cannot carry. An ABSENT field must
			// leave the stored mode alone, so an edit that only moves the
			// limit does not silently reset a policy chosen earlier.
			name: "an absent fail mode is not a request to clear one", body: `{"monthly_limit": 5}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if parsed.natsFailModeSet {
					t.Fatal("natsFailModeSet = true for an absent field: the upsert would write NULL")
				}
				if parsed.projectPolicyRequested() {
					t.Fatal("projectPolicyRequested() = true for a payload carrying neither field")
				}
			},
		},
		{
			// …and the one it must carry: an EXPLICIT null clears the override
			// back to the platform baseline.
			name: "an explicit null fail mode clears the override", body: `{"nats_fail_mode": null}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if !parsed.natsFailModeSet {
					t.Fatal("natsFailModeSet = false for an explicit null: the clear would be dropped")
				}
				if parsed.natsFailMode != nil {
					t.Fatalf("natsFailMode = %v, want nil", *parsed.natsFailMode)
				}
			},
		},
		{
			name: "a fail mode is matched case-insensitively", body: `{"nats_fail_mode": "Fail_Open"}`,
			check: func(t *testing.T, parsed parsedBudgetWrite) {
				if parsed.natsFailMode == nil || *parsed.natsFailMode != "fail_open" {
					t.Fatalf("natsFailMode = %v, want the normalised fail_open", parsed.natsFailMode)
				}
			},
		},
		{name: "a negative limit", body: `{"monthly_limit": -0.01}`, wantCode: http.StatusBadRequest},
		// A period the gateway does not compute. It has no CHECK constraint
		// behind it (0067 declares only a default), so this validator is the
		// only thing between the API and a stored value nothing bills against.
		{name: "an unenforceable period", body: `{"budget_period": "weekly"}`, wantCode: http.StatusBadRequest},
		{name: "an empty period", body: `{"budget_period": ""}`, wantCode: http.StatusBadRequest},
		// Outside 0067's CHECK. Refused here so it is a 400 naming the field
		// rather than a 23514 surfaced as a 500 — and because
		// failmode.ResolveFailMode would ignore it anyway, silently leaving the
		// project on the platform baseline.
		{name: "a fail mode outside the CHECK", body: `{"nats_fail_mode": "fail_sideways"}`, wantCode: http.StatusBadRequest},
		{name: "a non-string fail mode", body: `{"nats_fail_mode": 3}`, wantCode: http.StatusBadRequest},
		{name: "a non-numeric limit", body: `{"monthly_limit": "ten"}`, wantCode: http.StatusBadRequest},
		{name: "a foreign currency", body: `{"currency": "EUR"}`, wantCode: http.StatusBadRequest},
		{name: "a zero threshold", body: `{"soft_alert_pct": 0}`, wantCode: http.StatusBadRequest},
		{name: "a threshold above 100", body: `{"soft_alert_pct": 101}`, wantCode: http.StatusBadRequest},
		{name: "an unparseable body", body: `{`, wantCode: http.StatusBadRequest},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader([]byte(testCase.body)))
			recorder := httptest.NewRecorder()
			parsed, ok := decodeBudgetWrite(recorder, request)

			if testCase.wantCode != 0 {
				if ok {
					t.Fatal("payload was accepted, want a rejection")
				}
				if recorder.Code != testCase.wantCode {
					t.Fatalf("status = %d, want %d (body %s)",
						recorder.Code, testCase.wantCode, recorder.Body.String())
				}
				return
			}
			if !ok {
				t.Fatalf("payload was rejected: %s", recorder.Body.String())
			}
			testCase.check(t, parsed)
		})
	}
}

// A lowercase currency is the same currency. Rejecting it would fail a write
// nobody got wrong.
func TestDecodeBudgetWriteAcceptsCurrencyCaseInsensitively(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader([]byte(`{"currency":"usd"}`)))
	recorder := httptest.NewRecorder()
	if _, ok := decodeBudgetWrite(recorder, request); !ok {
		t.Fatalf("lowercase usd was rejected: %s", recorder.Body.String())
	}
}

// userBudgetScopeID has to be project-qualified: a bare user id would collide
// across projects on the accumulator's (scope, scope_id, period_start) key, so
// one member's spend in two projects would land on one row.
func TestUserBudgetScopeIDIsProjectQualified(t *testing.T) {
	if got := userBudgetScopeID(7, 42); got != "7:42" {
		t.Fatalf("scope id = %q, want 7:42", got)
	}
	if userBudgetScopeID(7, 42) == userBudgetScopeID(8, 42) {
		t.Fatal("the same user in two projects produced one accumulator key")
	}
}

// The redaction list is the money, and only the money. A field added to
// budgetState later is visible by default, which is the safe direction for a
// percentage and the wrong one for a cost — so this pins what "amount" means.
func TestRedactAmountsRemovesTheCostFieldsAndKeepsTheRest(t *testing.T) {
	payload := map[string]any{
		"spend": json.Number("1"), "monthly_limit": json.Number("2"),
		"effective_limit": json.Number("3"), "remaining": json.Number("4"),
		"currency": "USD", "percent_used": json.Number("50"),
		"warning_pct": 80, "period": "202608", "spend_available": true,
	}
	redactAmounts(payload)

	for _, gone := range []string{"spend", "monthly_limit", "effective_limit", "remaining", "currency"} {
		if _, present := payload[gone]; present {
			t.Fatalf("%s survived redaction", gone)
		}
	}
	for _, kept := range []string{"percent_used", "warning_pct", "period", "spend_available"} {
		if _, present := payload[kept]; !present {
			t.Fatalf("%s was removed; the usage bar needs it", kept)
		}
	}
}

package gateway

// The platform usage report's decisions, without a database.
//
// The failures worth pinning here are the ones that produce a PLAUSIBLE screen:
// a section that failed rendering as a section with nothing in it, a deleted
// project's spend vanishing from a breakdown that must still sum to the totals
// beside it, and a service account's spend attributed to a person.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scanning returns a scan func that copies the given values into the targets,
// standing in for one result row.
func scanning(values ...any) func(...any) error {
	return func(targets ...any) error {
		if len(targets) != len(values) {
			return errors.New("column count mismatch")
		}
		for index, target := range targets {
			switch destination := target.(type) {
			case *string:
				*destination = values[index].(string)
			case *int64:
				*destination = values[index].(int64)
			case *float64:
				*destination = values[index].(float64)
			default:
				return errors.New("unsupported scan target")
			}
		}
		return nil
	}
}

// TestAProjectWithNoNameRendersAsItsID.
//
// The statement LEFT JOINs `centry.project`, so a deleted project keeps its
// spend in the breakdown. That row must still say something true: the id is the
// only fact left about it, and a blank label or an invented "(deleted)" would
// leave an operator unable to tell which project the money went to. Dropping the
// row instead — an inner join — would make the breakdown fail to sum to the
// totals beside it, with no explanation on the screen.
func TestAProjectWithNoNameRendersAsItsID(t *testing.T) {
	slice, err := scanUsageProject(scanning(
		int64(42), "", int64(3), int64(10), int64(5), int64(15), 1.25,
	))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if slice.Label != "Project 42" || slice.Detail != "42" {
		t.Errorf("slice = %+v, want the id as the label and the detail", slice)
	}

	named, err := scanUsageProject(scanning(
		int64(42), "Acme", int64(3), int64(10), int64(5), int64(15), 1.25,
	))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The id stays in `detail` even when a name resolved: two projects can carry
	// the same display name, and an operator acting on the row needs the id.
	if named.Label != "Acme" || named.Detail != "42" {
		t.Errorf("slice = %+v, want the name as the label and the id as the detail", named)
	}
}

// TestAMemberWithNoNameRendersAsItsID — the same rule for members. The SQL
// prefers a non-empty name, falls back to the email, and this covers the case
// where neither resolved.
func TestAMemberWithNoNameRendersAsItsID(t *testing.T) {
	slice, err := scanUsageMember(scanning(
		int64(7), "", int64(1), int64(2), int64(3), int64(5), 0.5,
	))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if slice.Label != "User 7" || slice.Detail != "7" {
		t.Errorf("slice = %+v, want the id as the label and the detail", slice)
	}
}

// TestAModelSliceKeepsItsProviderSeparate — the key is unique across providers
// (two of them can serve `gpt-4o`) while the label stays the model alone, so the
// table does not repeat the provider in every cell of a column that already has
// it.
func TestAModelSliceKeepsItsProviderSeparate(t *testing.T) {
	slice, err := scanUsageModel(scanning(
		"openai", "gpt-4o", int64(9), int64(100), int64(50), int64(150), 2.5, "llm", int64(0), "",
	))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if slice.Key != "openai/gpt-4o" || slice.Label != "gpt-4o" || slice.Detail != "openai" {
		t.Errorf("slice = %+v, want a provider-qualified key and an unqualified label", slice)
	}
	if slice.Operation != "llm" || slice.Units != nil || slice.Unit != "" {
		t.Errorf("slice = %+v, want an llm row with no units", slice)
	}
}

// TestUsageModelsIncludeToolRowsWithOperation pins the tool half of the
// table (#1238): a metered operation is a row whose provider is the vendor
// and whose model is the operation, labelled as a tool and carrying units.
func TestUsageModelsIncludeToolRowsWithOperation(t *testing.T) {
	slice, err := scanUsageModel(scanning(
		"tavily", "web_search", int64(1), int64(0), int64(0), int64(0), 0.008, "tool", int64(1), "credit",
	))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if slice.Operation != "tool" || slice.Units == nil || *slice.Units != 1 || slice.Unit != "credit" ||
		slice.Label != "web_search" || slice.Detail != "tavily" || slice.Key != "tavily/web_search#tool" {
		t.Errorf("slice = %+v, want a tool row with its units", slice)
	}
	if !strings.Contains(usageModelsSQL, "GROUP BY provider, model, operation, unit") {
		t.Error("the model table does not group on the operation, so a tool row could merge into a model row")
	}
}

// TestUsageWithoutAPoolExplainsItselfRatherThanReportingNoSpend.
//
// This is the silent failure the whole section is shaped around: an empty usage
// table and a usage table that could not be read look identical, and "nothing
// was spent" is the reassuring one an operator would believe. Every collection
// is present and empty AND the reason is attached.
func TestUsageWithoutAPoolExplainsItselfRatherThanReportingNoSpend(t *testing.T) {
	handler := NewLLMProxyHandler(nil, nil)
	recorder := httptest.NewRecorder()
	handler.Usage(recorder, httptest.NewRequest(http.MethodGet, "/usage?window=7d", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an explained empty report", recorder.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("an unbacked usage read carried no reason; the empty tables read as 'no spend'")
	}
	// The window still round-trips, so the client's selector does not snap back
	// to the default on a failure.
	if string(body["window"]) != `"7d"` {
		t.Errorf("window = %s, want the requested one echoed", body["window"])
	}
	for _, collection := range []string{"daily", "models", "projects", "members"} {
		if string(body[collection]) != "[]" {
			t.Errorf("%s = %s, want an empty array rather than null", collection, body[collection])
		}
	}
	if _, ok := body["totals"]; !ok {
		t.Error("no totals object; the client would branch on undefined")
	}
}

// TestAnUnrecognisedWindowFallsBackRatherThanFailing — the window is a display
// choice, and the usage report must not be refused because a query string was
// mistyped. Same rule as the catalogue's.
func TestAnUnrecognisedWindowFallsBackRatherThanFailing(t *testing.T) {
	handler := NewLLMProxyHandler(nil, nil)
	recorder := httptest.NewRecorder()
	handler.Usage(recorder, httptest.NewRequest(http.MethodGet, "/usage?window=all-time", nil))

	var body struct {
		Window string `json:"window"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Window != defaultUsageWindow {
		t.Errorf("window = %q, want the default %q", body.Window, defaultUsageWindow)
	}
}

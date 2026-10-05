package gateway

// The request log read surface.
//
// The assertions are the ones that separate a useful log from a misleading one:
// which inputs are refused rather than ignored, what an unreadable read renders
// as, and that the retention figure this surface publishes is the one the
// gateway actually enforces.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func logRequest(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// TestAMalformedAddressingFilterIsRefusedRatherThanIgnored.
//
// The window is a DISPLAY choice and falls back when mistyped — hiding the log
// over a typo would be worse than showing the default range. `project_id` and
// `cursor` are ADDRESSING: silently dropping a malformed project id shows the
// operator every project's traffic when they asked for one, which is a
// different answer to a different question, presented as the one they asked.
func TestAMalformedAddressingFilterIsRefusedRatherThanIgnored(t *testing.T) {
	handler := NewLLMProxyHandler(nil, nil)

	for _, target := range []string{
		"/logs?project_id=abc",
		"/logs?project_id=-1",
		"/logs?project_id=0",
		"/logs?cursor=abc",
		"/logs?cursor=0",
	} {
		recorder := httptest.NewRecorder()
		handler.Logs(recorder, logRequest(target))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, recorder.Code)
		}
	}

	// …while a mistyped window falls back and still answers.
	recorder := httptest.NewRecorder()
	handler.Logs(recorder, logRequest("/logs?window=forever"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a mistyped window was refused: %d", recorder.Code)
	}
	var body struct {
		Window string `json:"window"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Window != defaultUsageWindow {
		t.Errorf("window = %q, want the default", body.Window)
	}
}

// TestAnUnbackedReadExplainsItselfRatherThanReportingNoTraffic.
//
// An empty log and a log that could not be read look identical, and "no
// requests" is the reassuring one — it would tell an operator investigating an
// outage that nothing was even being attempted.
func TestAnUnbackedReadExplainsItselfRatherThanReportingNoTraffic(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewLLMProxyHandler(nil, nil).Logs(recorder, logRequest("/logs"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an explained empty page", recorder.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("an unbacked read carried no reason; the empty page reads as 'no traffic'")
	}
	if string(body["items"]) != "[]" {
		t.Errorf("items = %s, want an empty array rather than null", body["items"])
	}
	if _, ok := body["summary"]; !ok {
		t.Error("no summary object; the client would branch on undefined")
	}
	// No cursor on a page that could not be read: offering one would invite the
	// client to page forward through a log it never reached.
	if _, ok := body["next_cursor"]; ok {
		t.Error("a failed read offered a next_cursor")
	}
}

// TestTheCursorIsAStringOnTheWire.
//
// `id` is a BIGSERIAL and a JavaScript number cannot hold its full range. A
// numeric cursor would start losing precision silently, at a magnitude no test
// would reach, and the symptom would be a log that repeats or skips a page.
func TestTheCursorIsAStringOnTheWire(t *testing.T) {
	encoded, err := json.Marshal(RequestLogRow{ID: "9007199254740993"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"id":"9007199254740993"`) {
		t.Errorf("id is not a string on the wire: %s", encoded)
	}
}

// TestTheStatementsCannotDisclosePayloadColumns.
//
// The table has no column a prompt could reach, and that is the guarantee this
// surface rests on. If a payload column were ever added, this read must not be
// the thing that starts publishing it — so the SELECT lists are pinned to the
// columns that exist rather than being a `SELECT *` that would widen silently.
func TestTheStatementsCannotDisclosePayloadColumns(t *testing.T) {
	for name, statement := range map[string]string{
		"page":    listLogsSQL,
		"summary": logSummarySQL,
	} {
		if strings.Contains(statement, "*") && !strings.Contains(statement, "count(*)") {
			t.Errorf("%s uses SELECT *; a column added later would be published without a decision", name)
		}
		for _, forbidden := range []string{"prompt", "completion_text", "request_body", "response_body", "message"} {
			// `prompt_tokens` is a count and is fine; a bare `prompt` is not.
			if regexp.MustCompile(`\b` + forbidden + `\b`).MatchString(statement) {
				t.Errorf("%s selects %q, which would be caller content", name, forbidden)
			}
		}
	}
}

// TestTheSummaryCoversTheWholeWindowNotThePage — the page is capped at
// logPageSize, so a count taken from it would report "100 requests" for any
// busy window and "100 failures" would be unreachable. The summary runs its own
// statement over the same predicates.
func TestTheSummaryCoversTheWholeWindowNotThePage(t *testing.T) {
	if !strings.Contains(logSummarySQL, "count(*)") {
		t.Error("the summary does not count")
	}
	if strings.Contains(logSummarySQL, "LIMIT") {
		t.Error("the summary is capped; it would report the page rather than the window")
	}
	// The two statements must narrow identically, or the summary describes a
	// different set of rows than the page it sits above.
	for _, predicate := range []string{
		"occurred_at >= now() - $1::interval",
		"($2::bigint IS NULL OR project_id = $2::bigint)",
		"($3::text = '' OR model = $3::text)",
		"($4::boolean = false OR status >= 400)",
	} {
		if !strings.Contains(listLogsSQL, predicate) {
			t.Errorf("the page statement lost the predicate %q", predicate)
		}
		if !strings.Contains(logSummarySQL, predicate) {
			t.Errorf("the summary statement lost the predicate %q", predicate)
		}
	}
}

// TestTheRetentionFigureMatchesWhatTheGatewayEnforces.
//
// requestLogRetentionDays is restated here because the gateway's constant lives
// in an `internal/` package of another module. A drift would put a number on
// the operator's screen that no writer honours — telling them the log goes back
// thirty days when it goes back seven, so an absent request reads as "it never
// happened".
func TestTheRetentionFigureMatchesWhatTheGatewayEnforces(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..",
		"elitea-llm-gateway", "internal", "requestlog", "requestlog.go"))
	if err != nil {
		t.Skipf("the gateway module is not checked out beside this one: %v", err)
	}

	// The constant is written as a duration expression, so the check is on the
	// day count inside it rather than on a formatted string.
	pattern := regexp.MustCompile(`RetentionWindow\s*=\s*(\d+)\s*\*\s*24\s*\*\s*time\.Hour`)
	match := pattern.FindStringSubmatch(string(source))
	if match == nil {
		t.Fatal("could not find requestlog.RetentionWindow; the restatement here cannot be checked")
	}
	if match[1] != strconv.Itoa(requestLogRetentionDays) {
		t.Errorf("the gateway retains request logs for %s days; this surface publishes %d",
			match[1], requestLogRetentionDays)
	}
}

package guardrail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// presidioServer mocks the Presidio Analyzer REST API (POST /analyze). It
// finds each needle in the request text and answers with Python string
// indices (code points), as Presidio does.
func presidioServer(t *testing.T, needles map[string]string) (*httptest.Server, *presidioRequest) {
	t.Helper()
	seen := &presidioRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analyze" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(seen)
		results := []map[string]any{}
		for needle, entity := range needles {
			i := strings.Index(seen.Text, needle)
			if i < 0 {
				continue
			}
			start := utf8.RuneCountInString(seen.Text[:i])
			results = append(results, map[string]any{
				"entity_type": entity, "start": start, "end": start + utf8.RuneCountInString(needle), "score": 0.85,
				"analysis_explanation": nil,
				"recognition_metadata": map[string]any{"recognizer_name": "EmailRecognizer", "recognizer_identifier": "x"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func presidioRow(t *testing.T, url, action string, extra map[string]any) *Row {
	t.Helper()
	d := map[string]any{"name": "pii", "type": TypePresidio, "action": action, "url": url + "/analyze"}
	for k, v := range extra {
		d[k] = v
	}
	return mustRow(t, rowData(StageOutput, d))
}

func TestPresidioMasksCodePointSpans(t *testing.T) {
	srv, seen := presidioServer(t, map[string]string{"jürgen@example.com": "EMAIL_ADDRESS", "078-05-1120": "US_SSN"})
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{presidioRow(t, srv.URL, ActionMask, map[string]any{
		"language": "de", "entities": []any{"EMAIL_ADDRESS", "US_SSN"}, "min_score": 0.5,
	})}, Env: adapterEnv(srv), Collector: col}
	in := "Grüße — mail jürgen@example.com, SSN 078-05-1120."
	out, blocked := p.Check(context.Background(), in)
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	want := "Grüße — mail " + MaskMarker("email_address") + ", SSN " + MaskMarker("us_ssn") + "."
	if out != want {
		t.Fatalf("masked = %q, want %q", out, want)
	}
	if seen.Language != "de" || seen.Text != in || seen.ScoreThreshold == nil || *seen.ScoreThreshold != 0.5 ||
		strings.Join(seen.Entities, ",") != "EMAIL_ADDRESS,US_SSN" {
		t.Fatalf("request = %+v", seen)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeMasked || strings.Join(v[0].Labels, ",") != "email_address,us_ssn" {
		t.Fatalf("verdicts = %+v", v)
	}
	assertNoContent(t, v, "jürgen", "078-05")
}

func TestPresidioDefaultsAndFilters(t *testing.T) {
	srv, seen := presidioServer(t, map[string]string{"Alice": "PERSON", "bob@example.com": "EMAIL_ADDRESS"})
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{presidioRow(t, srv.URL, ActionMask, map[string]any{
		"entities": []any{"EMAIL_ADDRESS"},
	})}, Env: adapterEnv(srv), Collector: NewCollector()}
	out, _ := p.Check(context.Background(), "Alice wrote to bob@example.com")
	// The mock ignores the entities filter; the adapter drops PERSON.
	if out != "Alice wrote to "+MaskMarker("email_address") {
		t.Fatalf("masked = %q", out)
	}
	if seen.Language != DefaultPresidioLanguage || seen.ScoreThreshold != nil {
		t.Fatalf("request = %+v", seen)
	}
}

func TestPresidioBadResponsesFail(t *testing.T) {
	for name, body := range map[string]string{
		"not a list":    `{"error":"x"}`,
		"span outside":  `[{"entity_type":"PERSON","start":0,"end":99,"score":0.9}]`,
		"no span":       `[{"entity_type":"PERSON","score":0.9}]`,
		"bad score":     `[{"entity_type":"PERSON","start":0,"end":1,"score":3}]`,
		"bad type name": `[{"entity_type":"BAD TYPE!","start":0,"end":1,"score":0.9}]`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		p := &Pipeline{Stage: StageOutput, Rows: []*Row{presidioRow(t, srv.URL, ActionMask, nil)}, Env: adapterEnv(srv), Collector: NewCollector()}
		if _, blocked := p.Check(context.Background(), "short"); blocked == nil || blocked.Outcome != OutcomeErrorClosed {
			t.Errorf("%s: did not fail closed: %+v", name, blocked)
		}
		srv.Close()
	}
}

func TestPresidioRowRefusals(t *testing.T) {
	for name, d := range map[string]map[string]any{
		"bad entity":   {"type": TypePresidio, "action": "mask", "url": "http://p:3000/analyze", "entities": []any{"email"}},
		"bad language": {"type": TypePresidio, "action": "mask", "url": "http://p:3000/analyze", "language": "English"},
		"bad score":    {"type": TypePresidio, "action": "mask", "url": "http://p:3000/analyze", "min_score": 1.5},
		"no url":       {"type": TypePresidio, "action": "mask"},
	} {
		if _, err := ParseRow("r", rowData(StageInput, d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

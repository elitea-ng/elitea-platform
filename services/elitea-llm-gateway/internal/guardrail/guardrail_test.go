package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A GitHub token shape (rule github-token). It is a test value, not a token.
const testToken = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"

func mustRow(t *testing.T, data map[string]any) *Row {
	t.Helper()
	row, err := ParseRow("row", data)
	if err != nil {
		t.Fatalf("ParseRow: %v", err)
	}
	return row
}

func rowData(stage string, detectors ...map[string]any) map[string]any {
	list := make([]any, 0, len(detectors))
	for _, d := range detectors {
		list = append(list, d)
	}
	return map[string]any{"guardrail": map[string]any{"stage": stage, "detectors": list}}
}

func TestParseRowRefusesBadRows(t *testing.T) {
	cases := map[string]map[string]any{
		"no stage":      {"guardrail": map[string]any{"detectors": []any{map[string]any{"type": "secrets", "action": "mask"}}}},
		"no detectors":  {"guardrail": map[string]any{"stage": "input"}},
		"bad action":    rowData("input", map[string]any{"type": "secrets", "action": "drop"}),
		"bad type":      rowData("input", map[string]any{"type": "llama", "action": "flag"}),
		"bad regex":     rowData("input", map[string]any{"type": "regex", "action": "flag", "patterns": []any{"(?<=x)y"}}),
		"empty match":   rowData("input", map[string]any{"type": "regex", "action": "flag", "patterns": []any{"a*"}}),
		"no pattern":    rowData("input", map[string]any{"type": "regex", "action": "flag"}),
		"raw secret":    rowData("input", map[string]any{"type": "http", "action": "flag", "url": "https://c.example", "auth_secret": "s3cret"}),
		"userinfo url":  rowData("input", map[string]any{"type": "http", "action": "flag", "url": "https://u:p@c.example"}),
		"unknown rule":  rowData("input", map[string]any{"type": "secrets", "action": "mask", "rules": []any{"nope"}}),
		"output embeds": {"guardrail": map[string]any{"stage": "output", "apply_to_embeddings": true, "detectors": []any{map[string]any{"type": "secrets", "action": "mask"}}}},
		"duplicate":     rowData("input", map[string]any{"type": "secrets", "action": "mask"}, map[string]any{"type": "secrets", "action": "flag"}),
	}
	for name, data := range cases {
		if _, err := ParseRow("r", data); err == nil {
			t.Errorf("%s: ParseRow accepted the row", name)
		}
	}
	long := make([]any, MaxPatterns+1)
	for i := range long {
		long[i] = "x"
	}
	if _, err := ParseRow("r", rowData("input", map[string]any{"type": "regex", "action": "flag", "patterns": long})); err == nil {
		t.Error("ParseRow accepted more than MaxPatterns patterns")
	}
}

func TestDefaultFailMode(t *testing.T) {
	block := mustRow(t, rowData("input", map[string]any{"type": "secrets", "action": "block"}))
	if block.FailMode != FailClosed {
		t.Errorf("block row fail mode = %s, want closed", block.FailMode)
	}
	flag := mustRow(t, rowData("input", map[string]any{"type": "secrets", "action": "flag"}))
	if flag.FailMode != FailOpen {
		t.Errorf("flag row fail mode = %s, want open", flag.FailMode)
	}
}

func TestCheckMasksSecretsAndTerms(t *testing.T) {
	row := mustRow(t, rowData("output",
		map[string]any{"type": "secrets", "action": "mask"},
		map[string]any{"name": "terms", "type": "regex", "action": "mask", "label": "project", "terms": []any{"Project Falcon"}, "patterns": []any{`EMP-\d{6}`}},
	))
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: col}
	out, blocked := p.Check(context.Background(), "token "+testToken+" for project falcon and EMP-123456.")
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	want := "token [REDACTED:github-token] for [REDACTED:project] and [REDACTED:project]."
	if out != want {
		t.Fatalf("masked = %q, want %q", out, want)
	}
	verdicts := col.Verdicts()
	if len(verdicts) != 2 || verdicts[0].Outcome != OutcomeMasked || verdicts[1].Findings != 2 {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	assertNoContent(t, verdicts, testToken, "falcon", "EMP-123456")
}

func TestCheckBlockStopsTheRow(t *testing.T) {
	row := mustRow(t, rowData("input",
		map[string]any{"type": "secrets", "action": "block"},
		map[string]any{"name": "later", "type": "regex", "action": "flag", "terms": []any{"x"}},
	))
	col := NewCollector()
	p := &Pipeline{Stage: StageInput, Rows: []*Row{row}, Collector: col}
	_, blocked := p.Check(context.Background(), "x "+testToken)
	if blocked == nil || blocked.Outcome != OutcomeBlocked || blocked.Labels[0] != "github-token" {
		t.Fatalf("blocked = %+v", blocked)
	}
	if len(col.Verdicts()) != 1 {
		t.Fatalf("the detector after the block ran: %+v", col.Verdicts())
	}
}

// TestStreamNeverReleasesRawMaskMatch feeds an answer that holds two secrets
// in two chunks, split at every byte position, and checks that the released
// text equals the non-streamed result and never holds the raw value.
func TestStreamNeverReleasesRawMaskMatch(t *testing.T) {
	row := mustRow(t, map[string]any{"guardrail": map[string]any{
		"stage": "output", "stream_window_bytes": 64.0,
		"detectors": []any{map[string]any{"type": "secrets", "action": "mask"}},
	}})
	answer := strings.Repeat("lorem ipsum ", 20) + testToken + " middle " + strings.Repeat("dolor ", 15) +
		"AKIA" + "IOSFODNN7ABCDEFG" + " end"
	ref := &Pipeline{Stage: StageOutput, Rows: []*Row{row}}
	want, _ := ref.Check(context.Background(), answer)
	for split := 1; split < len(answer); split++ {
		for _, size := range []int{1, 7, 64} {
			p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: NewCollector()}
			f := p.NewStream()
			var got strings.Builder
			chunks := chunk(answer[:split], size)
			chunks = append(chunks, chunk(answer[split:], size)...)
			for _, c := range chunks {
				out, blocked := f.Push(context.Background(), c)
				if blocked != nil {
					t.Fatalf("blocked: %+v", blocked)
				}
				got.WriteString(out)
				if strings.Contains(got.String(), testToken[:20]) || strings.Contains(got.String(), "AKIAIOSFODNN") {
					t.Fatalf("split %d size %d: raw secret released: %q", split, size, got.String())
				}
			}
			out, _ := f.Finish(context.Background())
			got.WriteString(out)
			if got.String() != want {
				t.Fatalf("split %d size %d: streamed %q, want %q", split, size, got.String(), want)
			}
			if f.Released() != want {
				t.Fatalf("Released() = %q", f.Released())
			}
		}
		if split > 400 {
			split += 3
		}
	}
}

func chunk(s string, size int) []string {
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func TestStreamBlockEndsTheStream(t *testing.T) {
	row := mustRow(t, map[string]any{"guardrail": map[string]any{
		"stage": "output", "stream_window_bytes": 64.0,
		"detectors": []any{map[string]any{"type": "secrets", "action": "block"}},
	}})
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: NewCollector()}
	f := p.NewStream()
	var released strings.Builder
	var blocked *Verdict
	for _, c := range chunk("hello "+testToken+strings.Repeat(" tail", 100), 5) {
		var out string
		out, blocked = f.Push(context.Background(), c)
		released.WriteString(out)
		if blocked != nil {
			break
		}
	}
	if blocked == nil {
		out, b := f.Finish(context.Background())
		released.WriteString(out)
		blocked = b
	}
	if blocked == nil || blocked.Outcome != OutcomeBlocked {
		t.Fatalf("no block: %+v", blocked)
	}
	if strings.Contains(released.String(), "ghp_") {
		t.Fatalf("the blocked secret was released: %q", released.String())
	}
}

func TestStreamFlagAddsNoDelay(t *testing.T) {
	row := mustRow(t, rowData("output", map[string]any{"type": "secrets", "action": "flag"}))
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: col}
	f := p.NewStream()
	if f.Buffers() {
		t.Fatal("a flag-only filter buffers")
	}
	out, _ := f.Push(context.Background(), "a "+testToken)
	if out != "a "+testToken {
		t.Fatalf("flag delayed or changed the text: %q", out)
	}
	if _, b := f.Finish(context.Background()); b != nil {
		t.Fatalf("flag blocked: %+v", b)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeFlagged {
		t.Fatalf("verdicts = %+v", v)
	}
}

// slowAdapter answers after delay with the given findings. Keep-alives are
// off so the test leaves no idle connection behind.
func slowAdapter(t *testing.T, delay time.Duration, resp AdapterResponse, seen *AdapterRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			_ = json.NewDecoder(r.Body).Decode(seen)
			seen.Content = r.Header.Get("Authorization")
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func adapterEnv(srv *httptest.Server) Env {
	return Env{
		HTTPClient:   &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		EgressAllows: func(u string) bool { return strings.HasPrefix(u, srv.URL) },
		ResolveSecret: func(_ context.Context, ref string) (string, error) {
			return "Bearer resolved-" + strings.TrimSuffix(strings.TrimPrefix(ref, "{{secret."), "}}"), nil
		},
	}
}

func adapterRow(t *testing.T, url, action, failMode string) *Row {
	t.Helper()
	g := map[string]any{
		"stage": "output", "timeout_ms": 100.0,
		"detectors": []any{map[string]any{"name": "classifier", "type": "http", "action": action, "url": url, "auth_secret": "{{secret.CLS}}"}},
	}
	if failMode != "" {
		g["fail_mode"] = failMode
	}
	return mustRow(t, map[string]any{"guardrail": g})
}

func TestAdapterTimeoutClosedRefuses(t *testing.T) {
	srv := slowAdapter(t, 2*time.Second, AdapterResponse{}, nil)
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{adapterRow(t, srv.URL, ActionBlock, FailClosed)}, Env: adapterEnv(srv), Collector: col}
	_, blocked := p.Check(context.Background(), "anything")
	if blocked == nil || blocked.Outcome != OutcomeErrorClosed || blocked.FailMode != FailClosed {
		t.Fatalf("closed timeout did not refuse: %+v", blocked)
	}
}

func TestAdapterTimeoutOpenProceedsAndFlags(t *testing.T) {
	srv := slowAdapter(t, 2*time.Second, AdapterResponse{}, nil)
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{adapterRow(t, srv.URL, ActionBlock, FailOpen)}, Env: adapterEnv(srv), Collector: col}
	out, blocked := p.Check(context.Background(), "anything")
	if blocked != nil || out != "anything" {
		t.Fatalf("open timeout did not proceed: %q %+v", out, blocked)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeErrorOpen || v[0].Labels[0] != LabelDetectorError {
		t.Fatalf("open timeout verdict = %+v", v)
	}
}

func TestAdapterContractAndSpans(t *testing.T) {
	start, end, score := 4, 9, 0.9
	var seen AdapterRequest
	srv := slowAdapter(t, 0, AdapterResponse{Findings: []AdapterFinding{{Label: "pii", Score: &score, Start: &start, End: &end}}}, &seen)
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{adapterRow(t, srv.URL, ActionMask, "")}, Env: adapterEnv(srv), Collector: NewCollector()}
	out, blocked := p.Check(context.Background(), "héllo Alice!")
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	// Code points 4..9 of "héllo Alice!" are "o Ali".
	if out != "héll[REDACTED:pii]ce!" {
		t.Fatalf("masked = %q", out)
	}
	if seen.Version != ContractVersion || seen.Stage != StageOutput || seen.Detector != "classifier" {
		t.Fatalf("request = %+v", seen)
	}
	if seen.Content != "Bearer resolved-CLS" {
		t.Fatalf("auth header = %q", seen.Content)
	}
}

func TestAdapterRefusedByEgress(t *testing.T) {
	srv := slowAdapter(t, 0, AdapterResponse{}, nil)
	env := adapterEnv(srv)
	env.EgressAllows = func(string) bool { return false }
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{adapterRow(t, srv.URL, ActionBlock, "")}, Env: env, Collector: NewCollector()}
	if _, blocked := p.Check(context.Background(), "x"); blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("an adapter off the egress allowlist was called: %+v", blocked)
	}
}

// TestSecretRulesArePinnedToThePlatformRuleSet fails when the embedded copy
// differs from the canonical rule file.
func TestSecretRulesArePinnedToThePlatformRuleSet(t *testing.T) {
	canonical, err := os.ReadFile("../../../../libs/rust/local-tools/src/handoff/secret_rules.json")
	if err != nil {
		t.Fatalf("read the canonical rule set: %v (if it moved, update this path; do not delete the pin)", err)
	}
	if !bytes.Equal(canonical, secretRulesJSON) {
		t.Fatal("internal/guardrail/secret_rules.json differs from libs/rust/local-tools/src/handoff/secret_rules.json; copy the canonical file here")
	}
	if len(SecretRuleIDs()) == 0 {
		t.Fatal("no secret rule compiled")
	}
}

// TestAdapterContractMatchesTheSchemas keeps the Go structs and the JSON
// schemas in libs/jsonschema/guardrail/v1 on the same field names.
func TestAdapterContractMatchesTheSchemas(t *testing.T) {
	read := func(name string) map[string]any {
		raw, err := os.ReadFile("../../../../libs/jsonschema/guardrail/v1/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return doc
	}
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	req := read("http-adapter-request.schema.json")
	if got := strings.Join(sorted(keys(req["properties"].(map[string]any))), ","); got != "content,detector,stage,version" {
		t.Fatalf("request schema properties = %s", got)
	}
	if req["properties"].(map[string]any)["version"].(map[string]any)["const"] != ContractVersion {
		t.Fatal("request schema version differs from ContractVersion")
	}
	resp := read("http-adapter-response.schema.json")
	items := resp["properties"].(map[string]any)["findings"].(map[string]any)["items"].(map[string]any)
	if got := strings.Join(sorted(keys(items["properties"].(map[string]any))), ","); got != "end,label,score,start" {
		t.Fatalf("finding schema properties = %s", got)
	}
	b, _ := json.Marshal(AdapterRequest{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if got := strings.Join(sorted(keys(m)), ","); got != "content,detector,stage,version" {
		t.Fatalf("AdapterRequest fields = %s", got)
	}
}

func sorted(in []string) []string {
	for i := range in {
		for j := i + 1; j < len(in); j++ {
			if in[j] < in[i] {
				in[i], in[j] = in[j], in[i]
			}
		}
	}
	return in
}

func assertNoContent(t *testing.T, verdicts []Verdict, values ...string) {
	t.Helper()
	raw, _ := json.Marshal(verdicts)
	for _, v := range values {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(v)) {
			t.Fatalf("a verdict carries content %q: %s", v, raw)
		}
	}
}

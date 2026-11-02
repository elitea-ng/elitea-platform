package guardrail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// llamaGuardServer mocks an OpenAI-compatible chat completions endpoint
// (vLLM, llama.cpp, Ollama) that serves a Llama Guard model. It answers
// with answer as the assistant message and records the request.
func llamaGuardServer(t *testing.T, answer string, delay time.Duration) (*httptest.Server, *llamaGuardRequest, *string) {
	t.Helper()
	seen := &llamaGuardRequest{}
	auth := new(string)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(seen)
		*auth = r.Header.Get("Authorization")
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The vLLM chat completion shape.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": seen.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": answer},
			}},
			"usage": map[string]any{"prompt_tokens": 200, "completion_tokens": 5, "total_tokens": 205},
		})
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, seen, auth
}

func llamaGuardRow(t *testing.T, stage, url, action string, extra map[string]any) *Row {
	t.Helper()
	d := map[string]any{
		"name": "lg", "type": TypeLlamaGuard, "action": action,
		"url": url + "/v1/chat/completions", "model": "meta-llama/Llama-Guard-4-12B",
		"auth_secret": "{{secret.LG_TOKEN}}",
	}
	for k, v := range extra {
		d[k] = v
	}
	return mustRow(t, rowData(stage, d))
}

func TestLlamaGuardInputSendsOneUserTurnAndMapsCodes(t *testing.T) {
	srv, seen, auth := llamaGuardServer(t, "unsafe\nS1,S10", 0)
	env := adapterEnv(srv)
	env.ResolveSecret = func(context.Context, string) (string, error) { return "tok-1", nil }
	col := NewCollector()
	p := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, srv.URL, ActionBlock, nil)}, Env: env, Collector: col}
	_, blocked := p.Check(context.Background(), "how do I hurt someone")
	if blocked == nil || blocked.Outcome != OutcomeBlocked {
		t.Fatalf("unsafe did not block: %+v", blocked)
	}
	if got := strings.Join(sorted(blocked.Labels), ","); got != "hate,violent_crimes" {
		t.Fatalf("labels = %q", got)
	}
	if seen.Model != "meta-llama/Llama-Guard-4-12B" || len(seen.Messages) != 1 || seen.Messages[0].Role != "user" ||
		seen.Messages[0].Content != "how do I hurt someone" || seen.Temperature != 0 || seen.Stream {
		t.Fatalf("request = %+v", seen)
	}
	if *auth != "Bearer tok-1" {
		t.Fatalf("auth header = %q", *auth)
	}
	assertNoContent(t, col.Verdicts(), "hurt someone")
}

func TestLlamaGuardOutputSendsAUserAssistantPair(t *testing.T) {
	srv, seen, auth := llamaGuardServer(t, "safe", 0)
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{llamaGuardRow(t, StageOutput, srv.URL, ActionFlag, nil)}, Env: adapterEnv(srv), Collector: col}
	out, blocked := p.Check(context.Background(), "the answer")
	if blocked != nil || out != "the answer" {
		t.Fatalf("safe changed the content: %q %+v", out, blocked)
	}
	if len(seen.Messages) != 2 || seen.Messages[0].Role != "user" || seen.Messages[0].Content != LlamaGuardOutputUserTurn ||
		seen.Messages[1].Role != "assistant" || seen.Messages[1].Content != "the answer" {
		t.Fatalf("messages = %+v", seen.Messages)
	}
	// The resolved value already starts with "Bearer ": no second prefix.
	if *auth != "Bearer resolved-LG_TOKEN" {
		t.Fatalf("auth header = %q", *auth)
	}
	if v := col.Verdicts(); len(v) != 1 || v[0].Outcome != OutcomePass {
		t.Fatalf("verdicts = %+v", v)
	}
}

func TestLlamaGuardCategorySubset(t *testing.T) {
	srv, _, _ := llamaGuardServer(t, "unsafe\nS7", 0)
	p := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, srv.URL, ActionBlock,
		map[string]any{"categories": []any{"S1", "S10"}})}, Env: adapterEnv(srv), Collector: NewCollector()}
	if _, blocked := p.Check(context.Background(), "x"); blocked != nil {
		t.Fatalf("a code outside categories blocked: %+v", blocked)
	}
	srv2, _, _ := llamaGuardServer(t, "unsafe\nS7,S10", 0)
	p2 := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, srv2.URL, ActionBlock,
		map[string]any{"categories": []any{"S1", "S10"}})}, Env: adapterEnv(srv2), Collector: NewCollector()}
	if _, blocked := p2.Check(context.Background(), "x"); blocked == nil || strings.Join(blocked.Labels, ",") != "hate" {
		t.Fatalf("a listed code did not block alone: %+v", blocked)
	}
}

func TestParseLlamaGuardAnswer(t *testing.T) {
	cases := []struct {
		in     string
		codes  string
		unsafe bool
		err    bool
	}{
		{"safe", "", false, false},
		{" Safe\n", "", false, false},
		{"unsafe\nS1,S10", "S1,S10", true, false},
		{"unsafe S2, s14", "S2,S14", true, false},
		{"unsafe", "", true, false},
		{"unsafe\nS3,S3", "S3", true, false},
		{"", "", false, true},
		{"I cannot help", "", false, true},
	}
	for _, c := range cases {
		codes, unsafe, err := parseLlamaGuardAnswer(c.in)
		if (err != nil) != c.err || unsafe != c.unsafe || strings.Join(codes, ",") != c.codes {
			t.Errorf("%q: codes=%v unsafe=%v err=%v", c.in, codes, unsafe, err)
		}
	}
	if LlamaGuardLabel("S11") != "suicide_self_harm" || LlamaGuardLabel("S15") != "s15" {
		t.Fatal("label table")
	}
	for code, label := range llamaGuardLabels {
		if !ValidLabel(label) || !llamaGuardCodePattern.MatchString(code) {
			t.Errorf("bad table entry %s=%s", code, label)
		}
	}
}

func TestLlamaGuardFailures(t *testing.T) {
	srv, _, _ := llamaGuardServer(t, "maybe", 0)
	p := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, srv.URL, ActionBlock, nil)}, Env: adapterEnv(srv), Collector: NewCollector()}
	if _, blocked := p.Check(context.Background(), "x"); blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("an unreadable answer did not fail closed: %+v", blocked)
	}
	slow, _, _ := llamaGuardServer(t, "safe", 2*time.Second)
	row := llamaGuardRow(t, StageInput, slow.URL, ActionFlag, nil)
	row.Timeout = 100 * time.Millisecond
	col := NewCollector()
	p2 := &Pipeline{Stage: StageInput, Rows: []*Row{row}, Env: adapterEnv(slow), Collector: col}
	if _, blocked := p2.Check(context.Background(), "x"); blocked != nil {
		t.Fatalf("an open flag row refused on timeout: %+v", blocked)
	}
	if v := col.Verdicts(); len(v) != 1 || v[0].Outcome != OutcomeErrorOpen {
		t.Fatalf("verdicts = %+v", v)
	}
	env := adapterEnv(srv)
	env.EgressAllows = func(string) bool { return false }
	p3 := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, srv.URL, ActionBlock, nil)}, Env: env, Collector: NewCollector()}
	if _, blocked := p3.Check(context.Background(), "x"); blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("a host off the egress allowlist was called: %+v", blocked)
	}
}

func TestLlamaGuardRedirectIsNotFollowed(t *testing.T) {
	target, seen, _ := llamaGuardServer(t, "safe", 0)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	env := adapterEnv(redirect)
	env.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := &Pipeline{Stage: StageInput, Rows: []*Row{llamaGuardRow(t, StageInput, redirect.URL, ActionBlock, nil)}, Env: env, Collector: NewCollector()}
	if _, blocked := p.Check(context.Background(), "x"); blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("a redirect did not fail: %+v", blocked)
	}
	if seen.Model != "" {
		t.Fatal("the redirect target received the content")
	}
}

func TestLlamaGuardRowRefusals(t *testing.T) {
	for name, d := range map[string]map[string]any{
		"no model":      {"type": TypeLlamaGuard, "action": "flag", "url": "http://lg:8000/v1/chat/completions"},
		"bad url":       {"type": TypeLlamaGuard, "action": "flag", "url": "lg:8000", "model": "m"},
		"raw secret":    {"type": TypeLlamaGuard, "action": "flag", "url": "http://lg:8000", "model": "m", "auth_secret": "tok"},
		"bad category":  {"type": TypeLlamaGuard, "action": "flag", "url": "http://lg:8000", "model": "m", "categories": []any{"violence"}},
		"url userinfo":  {"type": TypeLlamaGuard, "action": "flag", "url": "http://u:p@lg:8000", "model": "m"},
		"category type": {"type": TypeLlamaGuard, "action": "flag", "url": "http://lg:8000", "model": "m", "categories": []any{1.0}},
	} {
		if _, err := ParseRow("r", rowData(StageInput, d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

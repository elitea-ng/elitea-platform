package guardrail

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// azureMock is a mock of the Content Safety 2024-09-01 text API. It answers
// with the shapes of the public REST reference.
type azureMock struct {
	mu       sync.Mutex
	analyze  []AzureAnalyzeRequest
	shield   []AzureShieldRequest
	keys     []string
	analyzed string // the text:analyze response body
	shielded string // the text:shieldPrompt response body
	status   int
}

func (m *azureMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Query().Get("api-version") != azureAPIVersion {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		m.keys = append(m.keys, r.Header.Get(azureKeyHeader))
		if m.status != 0 {
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"error":{"code":"InvalidRequestBody","message":"The text is too long."}}`))
			return
		}
		switch r.URL.Path {
		case "/contentsafety/text:analyze":
			var req AzureAnalyzeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.analyze = append(m.analyze, req)
			_, _ = w.Write([]byte(m.analyzed))
		case "/contentsafety/text:shieldPrompt":
			var req AzureShieldRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.shield = append(m.shield, req)
			_, _ = w.Write([]byte(m.shielded))
		default:
			t.Errorf("path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func azureDetectorObj(url string) map[string]any {
	return map[string]any{
		"name": "azure", "type": TypeAzureContentSafety, "action": "flag", "endpoint": url + "/",
		"api_key_secret": "{{secret.AZURE_CS_KEY}}",
		"categories":     map[string]any{"Hate": 4.0, "Violence": 4.0},
		"blocklists":     []any{"codenames"},
		"prompt_shields": true,
	}
}

func TestAzureAnalyzeAndShieldMapToFindings(t *testing.T) {
	m := &azureMock{
		analyzed: `{"blocklistsMatch":[{"blocklistName":"codenames","blocklistItemId":"1","blocklistItemText":"falcon"}],
			"categoriesAnalysis":[{"category":"Hate","severity":4},{"category":"Violence","severity":2}]}`,
		shielded: `{"userPromptAnalysis":{"attackDetected":true},"documentsAnalysis":[]}`,
	}
	srv := cloudServer(t, m.handler(t))
	p, col := cloudPipeline(t, StageInput, cloudEnv(srv), azureDetectorObj(srv.URL))
	if _, blocked := p.Check(context.Background(), "some prompt"); blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeFlagged {
		t.Fatalf("verdicts = %+v", v)
	}
	got := strings.Join(v[0].Labels, ",")
	if got != "blocklist,hate,prompt_injection" {
		t.Fatalf("labels = %s", got)
	}
	if v[0].MaxScore != 1 {
		t.Fatalf("max score = %v", v[0].MaxScore)
	}
	if len(m.analyze) != 1 || m.analyze[0].Text != "some prompt" || m.analyze[0].OutputType != azureOutputFour ||
		strings.Join(m.analyze[0].Categories, ",") != "Hate,Violence" || m.analyze[0].BlocklistNames[0] != "codenames" {
		t.Fatalf("analyze request = %+v", m.analyze)
	}
	if len(m.shield) != 1 || m.shield[0].UserPrompt != "some prompt" || len(m.shield[0].Documents) != 0 {
		t.Fatalf("shield request = %+v", m.shield)
	}
	for _, k := range m.keys {
		if k != "value-AZURE_CS_KEY" {
			t.Fatalf("key header = %q", k)
		}
	}
}

func TestAzureSeverityScoreIsNormalised(t *testing.T) {
	m := &azureMock{analyzed: `{"blocklistsMatch":[],"categoriesAnalysis":[{"category":"SelfHarm","severity":7}]}`}
	srv := cloudServer(t, m.handler(t))
	d, err := newAzureDetector(map[string]any{
		"endpoint": srv.URL, "api_key_secret": "{{secret.K}}", "output_type": azureOutputEight,
		"categories": map[string]any{"SelfHarm": 3.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := d.detect(context.Background(), cloudEnv(srv), StageInput, "x")
	if err != nil || len(found) != 1 || found[0].Label != "self_harm" || found[0].Score != 1 || !found[0].Whole {
		t.Fatalf("findings = %+v, %v", found, err)
	}
	if m.analyze[0].OutputType != azureOutputEight {
		t.Fatalf("outputType = %q", m.analyze[0].OutputType)
	}
}

func TestAzureOutputStageShieldsAsDocument(t *testing.T) {
	m := &azureMock{shielded: `{"documentsAnalysis":[{"attackDetected":true}]}`}
	srv := cloudServer(t, m.handler(t))
	obj := map[string]any{
		"type": TypeAzureContentSafety, "action": "block", "endpoint": srv.URL,
		"api_key_secret": "{{secret.K}}", "prompt_shields": true,
	}
	p, _ := cloudPipeline(t, StageOutput, cloudEnv(srv), obj)
	_, blocked := p.Check(context.Background(), "answer")
	if blocked == nil || blocked.Outcome != OutcomeBlocked || blocked.Labels[0] != LabelPromptInjection {
		t.Fatalf("blocked = %+v", blocked)
	}
	if len(m.analyze) != 0 || len(m.shield) != 1 || m.shield[0].UserPrompt != "" || m.shield[0].Documents[0] != "answer" {
		t.Fatalf("requests = %+v %+v", m.analyze, m.shield)
	}
}

func TestAzureSplitsLongContentByCodePoints(t *testing.T) {
	m := &azureMock{analyzed: `{"categoriesAnalysis":[{"category":"Hate","severity":6}]}`}
	srv := cloudServer(t, m.handler(t))
	obj := map[string]any{
		"type": TypeAzureContentSafety, "action": "mask", "endpoint": srv.URL,
		"api_key_secret": "{{secret.K}}", "categories": map[string]any{"Hate": 2.0},
	}
	// 10 001 two-byte characters: two parts, 10 000 and 1 code points.
	content := strings.Repeat("é", azureMaxCodePoints+1)
	p, col := cloudPipeline(t, StageInput, cloudEnv(srv), obj)
	out, blocked := p.Check(context.Background(), content)
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	if len(m.analyze) != 2 || utf8.RuneCountInString(m.analyze[0].Text) != azureMaxCodePoints ||
		m.analyze[1].Text != "é" {
		t.Fatalf("parts sent = %d", len(m.analyze))
	}
	// Both parts are masked; the merged span is one marker.
	if out != MaskMarker("hate") {
		t.Fatalf("masked = %q", out[:min(len(out), 40)])
	}
	if v := col.Verdicts(); v[0].Findings != 2 {
		t.Fatalf("findings = %d", v[0].Findings)
	}
}

func TestAzureRefusesTooLongContent(t *testing.T) {
	m := &azureMock{}
	srv := cloudServer(t, m.handler(t))
	d, _ := newAzureDetector(map[string]any{"endpoint": srv.URL, "api_key_secret": "{{secret.K}}", "prompt_shields": true})
	_, err := d.detect(context.Background(), cloudEnv(srv), StageInput, strings.Repeat("a", azureMaxParts*azureMaxCodePoints+1))
	if err == nil || len(m.shield) != 0 {
		t.Fatalf("over-long content: err %v, calls %d", err, len(m.shield))
	}
}

func TestAzureErrorsFailTheRow(t *testing.T) {
	m := &azureMock{status: http.StatusUnauthorized}
	srv := cloudServer(t, m.handler(t))
	p, _ := cloudPipeline(t, StageInput, cloudEnv(srv), azureDetectorObj(srv.URL))
	// A flag-only row fails open.
	if out, blocked := p.Check(context.Background(), "x"); blocked != nil || out != "x" {
		t.Fatalf("open row: %q %+v", out, blocked)
	}
	obj := azureDetectorObj(srv.URL)
	obj["action"] = "block"
	p, _ = cloudPipeline(t, StageInput, cloudEnv(srv), obj)
	_, blocked := p.Check(context.Background(), "x")
	if blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("closed row: %+v", blocked)
	}
	// The error never names the content or the vendor body.
	d, _ := newAzureDetector(obj)
	_, err := d.detect(context.Background(), cloudEnv(srv), StageInput, "secret prompt")
	if err == nil || strings.Contains(err.Error(), "secret prompt") || strings.Contains(err.Error(), "too long") {
		t.Fatalf("error = %v", err)
	}
}

func TestAzureRefusedByEgress(t *testing.T) {
	m := &azureMock{}
	srv := cloudServer(t, m.handler(t))
	env := cloudEnv(srv)
	env.EgressAllows = func(string) bool { return false }
	d, _ := newAzureDetector(azureDetectorObj(srv.URL))
	if _, err := d.detect(context.Background(), env, StageInput, "x"); err == nil || len(m.keys) != 0 {
		t.Fatalf("an endpoint off the allowlist was called: %v", err)
	}
}

func TestAzureRefusesBadConfig(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"endpoint": "https://cs.example.com", "api_key_secret": "{{secret.K}}", "prompt_shields": true}
	}
	bad := map[string]func(map[string]any){
		"no key":          func(o map[string]any) { delete(o, "api_key_secret") },
		"raw key":         func(o map[string]any) { o["api_key_secret"] = "abc123" },
		"http endpoint":   func(o map[string]any) { o["endpoint"] = "http://cs.example.com" },
		"nothing to run":  func(o map[string]any) { delete(o, "prompt_shields") },
		"bad category":    func(o map[string]any) { o["categories"] = map[string]any{"Spam": 2.0} },
		"threshold 0":     func(o map[string]any) { o["categories"] = map[string]any{"Hate": 0.0} },
		"threshold 7 /4":  func(o map[string]any) { o["categories"] = map[string]any{"Hate": 7.0} },
		"bad output type": func(o map[string]any) { o["output_type"] = "Three" },
		"bad blocklist":   func(o map[string]any) { o["blocklists"] = []any{"has space"} },
		"shields string":  func(o map[string]any) { o["prompt_shields"] = "yes" },
	}
	for name, mutate := range bad {
		o := base()
		mutate(o)
		if _, err := newAzureDetector(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

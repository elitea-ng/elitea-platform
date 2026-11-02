package guardrail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// bedrockMock is a mock of bedrock-runtime ApplyGuardrail. It answers with
// the shapes of the public API reference.
type bedrockMock struct {
	mu       sync.Mutex
	reqs     []BedrockApplyRequest
	paths    []string
	auth     []string
	tokens   []string
	hashOK   bool
	response string
	status   int
}

func (m *bedrockMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(raw)
		m.hashOK = r.Header.Get("x-amz-content-sha256") == hex.EncodeToString(sum[:])
		m.paths = append(m.paths, r.URL.EscapedPath())
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		m.tokens = append(m.tokens, r.Header.Get("X-Amz-Security-Token"))
		if m.status != 0 {
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"message":"The security token included in the request is invalid."}`))
			return
		}
		var req BedrockApplyRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		m.reqs = append(m.reqs, req)
		_, _ = w.Write([]byte(m.response))
	}
}

func bedrockObj(endpoint string) map[string]any {
	return map[string]any{
		"name": "bedrock", "type": TypeBedrockGuardrails, "action": "mask",
		"region": "eu-west-1", "guardrail_identifier": "abc123def", "guardrail_version": "2",
		"endpoint":                 endpoint,
		"access_key_id_secret":     "{{secret.AWS_KEY_ID}}",
		"secret_access_key_secret": "{{secret.AWS_SECRET}}",
		"session_token_secret":     "{{secret.AWS_TOKEN}}",
	}
}

const bedrockAssessment = `{
  "action": "GUARDRAIL_INTERVENED",
  "actionReason": "Guardrail blocked.",
  "outputs": [{"text": "Sorry."}],
  "assessments": [{
    "contentPolicy": {"filters": [
      {"type": "PROMPT_ATTACK", "confidence": "HIGH", "filterStrength": "HIGH", "action": "BLOCKED", "detected": true},
      {"type": "HATE", "confidence": "LOW", "filterStrength": "HIGH", "action": "NONE", "detected": false},
      {"type": "INSULTS", "confidence": "MEDIUM", "filterStrength": "MEDIUM", "action": "BLOCKED"}
    ]},
    "topicPolicy": {"topics": [{"name": "Investment advice", "type": "DENY", "action": "BLOCKED", "detected": true}]},
    "wordPolicy": {
      "customWords": [{"match": "FALCON", "action": "BLOCKED", "detected": true}],
      "managedWordLists": [{"match": "darn", "type": "PROFANITY", "action": "NONE", "detected": false}]
    },
    "sensitiveInformationPolicy": {
      "piiEntities": [{"match": "jane@example.com", "type": "EMAIL", "action": "ANONYMIZED", "detected": true}],
      "regexes": [{"name": "Employee Id", "match": "EMP-123456", "regex": "EMP-\\d{6}", "action": "ANONYMIZED", "detected": true}]
    },
    "contextualGroundingPolicy": {"filters": [{"type": "GROUNDING", "threshold": 0.7, "score": 0.1, "action": "BLOCKED", "detected": true}]},
    "invocationMetrics": {"guardrailProcessingLatency": 120}
  }]
}`

func TestBedrockApplyGuardrailMapsAssessments(t *testing.T) {
	m := &bedrockMock{response: bedrockAssessment}
	srv := cloudServer(t, m.handler(t))
	p, col := cloudPipeline(t, StageOutput, cloudEnv(srv), bedrockObj(srv.URL))
	content := "Mail jane@example.com about project falcon, id EMP-123456."
	out, blocked := p.Check(context.Background(), content)
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	v := col.Verdicts()
	got := strings.Join(v[0].Labels, ",")
	want := "blocked_word,denied_topic,insults,pii:email,prompt_injection,regex:employee_id"
	if got != want {
		t.Fatalf("labels = %s, want %s", got, want)
	}
	// Whole findings mask everything; check the spans on a flag row instead.
	if out != MaskMarker(SlotPromptInjection) {
		t.Fatalf("masked = %q", out)
	}
	if len(m.reqs) != 1 || m.reqs[0].Source != "OUTPUT" || m.reqs[0].Content[0].Text.Text != content {
		t.Fatalf("request = %+v", m.reqs)
	}
	if m.paths[0] != "/guardrail/abc123def/version/2/apply" {
		t.Fatalf("path = %s", m.paths[0])
	}
	if !strings.HasPrefix(m.auth[0], "AWS4-HMAC-SHA256 Credential=value-AWS_KEY_ID/") ||
		!strings.Contains(m.auth[0], "/eu-west-1/bedrock/aws4_request") || !m.hashOK {
		t.Fatalf("signature = %q, payload hash ok %v", m.auth[0], m.hashOK)
	}
	if m.tokens[0] != "value-AWS_TOKEN" {
		t.Fatalf("session token = %q", m.tokens[0])
	}
}

func TestBedrockSpansFromMatchText(t *testing.T) {
	var resp BedrockApplyResponse
	if err := json.Unmarshal([]byte(bedrockAssessment), &resp); err != nil {
		t.Fatal(err)
	}
	content := "Mail jane@example.com about project falcon, id EMP-123456."
	byLabel := map[string]Finding{}
	for _, f := range bedrockFindings(&resp, content) {
		byLabel[f.Label] = f
	}
	for label, text := range map[string]string{"pii:email": "jane@example.com", "regex:employee_id": "EMP-123456", "blocked_word": "falcon"} {
		f := byLabel[label]
		if f.Whole || content[f.Start:f.End] != text {
			t.Errorf("%s: finding %+v", label, f)
		}
	}
	if f := byLabel[SlotPromptInjection]; !f.Whole || f.Score != 1 {
		t.Errorf("prompt attack: %+v", f)
	}
	if f := byLabel["insults"]; f.Score != 0.66 {
		t.Errorf("insults score = %v", f.Score)
	}
	if _, ok := byLabel["grounding"]; ok {
		t.Error("contextual grounding was mapped")
	}
}

func TestBedrockInterventionWithoutAssessmentIsAFinding(t *testing.T) {
	resp := BedrockApplyResponse{Action: "GUARDRAIL_INTERVENED"}
	f := bedrockFindings(&resp, "x")
	if len(f) != 1 || f[0].Label != "bedrock_intervened" || !f[0].Whole {
		t.Fatalf("findings = %+v", f)
	}
	if f := bedrockFindings(&BedrockApplyResponse{Action: "NONE"}, "x"); len(f) != 0 {
		t.Fatalf("NONE gave findings %+v", f)
	}
}

func TestBedrockInputStageAndARN(t *testing.T) {
	m := &bedrockMock{response: `{"action":"NONE","assessments":[{}],"outputs":[]}`}
	srv := cloudServer(t, m.handler(t))
	obj := bedrockObj(srv.URL)
	obj["guardrail_identifier"] = "arn:aws:bedrock:eu-west-1:123456789012:guardrail/abc123def"
	obj["guardrail_version"] = "DRAFT"
	delete(obj, "session_token_secret")
	p, col := cloudPipeline(t, StageInput, cloudEnv(srv), obj)
	if _, blocked := p.Check(context.Background(), "hello"); blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	if col.Verdicts()[0].Outcome != OutcomePass {
		t.Fatalf("verdict = %+v", col.Verdicts())
	}
	if m.reqs[0].Source != "INPUT" || m.tokens[0] != "" {
		t.Fatalf("request = %+v, token %q", m.reqs[0], m.tokens[0])
	}
	if m.paths[0] != "/guardrail/arn:aws:bedrock:eu-west-1:123456789012:guardrail%2Fabc123def/version/DRAFT/apply" {
		t.Fatalf("path = %s", m.paths[0])
	}
}

func TestBedrockErrorStatusFailsClosed(t *testing.T) {
	m := &bedrockMock{status: http.StatusForbidden}
	srv := cloudServer(t, m.handler(t))
	p, _ := cloudPipeline(t, StageInput, cloudEnv(srv), bedrockObj(srv.URL))
	_, blocked := p.Check(context.Background(), "x")
	if blocked == nil || blocked.Outcome != OutcomeErrorClosed {
		t.Fatalf("blocked = %+v", blocked)
	}
}

func TestBedrockSecretFailureCallsNothing(t *testing.T) {
	m := &bedrockMock{}
	srv := cloudServer(t, m.handler(t))
	env := cloudEnv(srv)
	env.ResolveSecret = nil
	d, err := newBedrockDetector(bedrockObj(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.detect(context.Background(), env, StageInput, "x"); err == nil || len(m.paths) != 0 {
		t.Fatalf("err %v, calls %d", err, len(m.paths))
	}
}

func TestBedrockDefaultEndpointAndBadConfig(t *testing.T) {
	obj := bedrockObj("")
	delete(obj, "endpoint")
	d, err := newBedrockDetector(obj)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.(*bedrockDetector).target(); got != "https://bedrock-runtime.eu-west-1.amazonaws.com/guardrail/abc123def/version/2/apply" {
		t.Fatalf("target = %s", got)
	}
	bad := map[string]func(map[string]any){
		"region with slash": func(o map[string]any) { o["region"] = "us-east-1.evil.com/x" },
		"no region":         func(o map[string]any) { delete(o, "region") },
		"bad id":            func(o map[string]any) { o["guardrail_identifier"] = "../x" },
		"bad version":       func(o map[string]any) { o["guardrail_version"] = "0" },
		"raw key":           func(o map[string]any) { o["access_key_id_secret"] = "AKIAEXAMPLE" },
		"no secret key":     func(o map[string]any) { delete(o, "secret_access_key_secret") },
		"http endpoint":     func(o map[string]any) { o["endpoint"] = "http://vpce.example.com" },
	}
	for name, mutate := range bad {
		o := bedrockObj("https://vpce.example.com")
		mutate(o)
		if _, err := newBedrockDetector(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

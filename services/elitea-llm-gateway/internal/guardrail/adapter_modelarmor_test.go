package guardrail

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// modelArmorMock is a mock of the Model Armor v1 sanitize methods and of the
// Google OAuth token endpoint. It answers with the shapes of the public REST
// reference.
type modelArmorMock struct {
	mu          sync.Mutex
	paths       []string
	bodies      []ModelArmorRequest
	auth        []string
	tokenCalls  int
	response    string
	status      int
	tokenStatus int
	expiresIn   int // seconds; 0 means 3600
}

func (m *modelArmorMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.URL.Path == "/token" {
			m.tokenCalls++
			if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.PostForm.Get("assertion") == "" {
				t.Errorf("token request: %v %v", err, r.PostForm)
			}
			if m.tokenStatus != 0 {
				w.WriteHeader(m.tokenStatus)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			expires := m.expiresIn
			if expires == 0 {
				expires = 3600
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"sa-token","token_type":"Bearer","expires_in":%d}`, expires)
			return
		}
		m.paths = append(m.paths, r.URL.Path)
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		var body ModelArmorRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.bodies = append(m.bodies, body)
		if m.status != 0 {
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`))
			return
		}
		_, _ = w.Write([]byte(m.response))
	}
}

const modelArmorTemplateName = "projects/acme-prod/locations/us-central1/templates/strict"

// serviceAccountKey returns a service-account key document with a fresh RSA
// key and the mock's token endpoint.
func serviceAccountKey(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme-prod", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "guardrail@acme-prod.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": tokenURI,
	})
	return string(doc)
}

func modelArmorEnv(t *testing.T, srv *httptest.Server) Env {
	env := cloudEnv(srv)
	doc := serviceAccountKey(t, srv.URL+"/token")
	env.ResolveSecret = func(_ context.Context, ref string) (string, error) {
		if ref != "{{secret.MODEL_ARMOR_SA}}" {
			return "", errors.New("unknown secret")
		}
		return doc, nil
	}
	return env
}

func modelArmorObj(endpoint string) map[string]any {
	return map[string]any{
		"name": "armor", "type": TypeModelArmor, "action": "flag", "template": modelArmorTemplateName,
		"endpoint": endpoint, "credentials_secret": "{{secret.MODEL_ARMOR_SA}}",
	}
}

const modelArmorMatch = `{
  "sanitizationResult": {
    "filterMatchState": "MATCH_FOUND",
    "invocationResult": "SUCCESS",
    "filterResults": {
      "rai": {"raiFilterResult": {"executionState": "EXECUTION_SUCCESS", "matchState": "MATCH_FOUND",
        "raiFilterTypeResults": {
          "sexually_explicit": {"matchState": "NO_MATCH_FOUND"},
          "hate_speech": {"confidenceLevel": "MEDIUM_AND_ABOVE", "matchState": "MATCH_FOUND"},
          "harassment": {"matchState": "NO_MATCH_FOUND"},
          "dangerous": {"matchState": "NO_MATCH_FOUND"}}}},
      "pi_and_jailbreak": {"piAndJailbreakFilterResult": {"executionState": "EXECUTION_SUCCESS", "matchState": "MATCH_FOUND", "confidenceLevel": "HIGH"}},
      "sdp": {"sdpFilterResult": {"inspectResult": {"executionState": "EXECUTION_SUCCESS", "matchState": "MATCH_FOUND",
        "findings": [
          {"infoType": "EMAIL_ADDRESS", "likelihood": "LIKELY", "location": {"byteRange": {"start": "10", "end": "24"}, "codepointRange": {"start": "0", "end": "5"}}},
          {"infoType": "PHONE_NUMBER", "likelihood": "VERY_LIKELY", "location": {"codepointRange": {"start": "28", "end": "36"}}}
        ]}}},
      "malicious_uris": {"maliciousUriFilterResult": {"executionState": "EXECUTION_SUCCESS", "matchState": "MATCH_FOUND",
        "maliciousUriMatchedItems": [{"uri": "https://bad.example", "locations": [{"start": "40", "end": "59"}]}]}},
      "csam": {"csamFilterFilterResult": {"executionState": "EXECUTION_SUCCESS", "matchState": "NO_MATCH_FOUND"}}
    }
  }
}`

// The content of the match fixture. "é" is two bytes, so byte and code point
// offsets differ by one after it.
const modelArmorContent = "Écris à joe@example.co, tel 555-0100 de https://bad.example now"

func TestModelArmorKeyModeMapsFilterResults(t *testing.T) {
	m := &modelArmorMock{response: modelArmorMatch}
	srv := cloudServer(t, m.handler(t))
	d, err := newModelArmorDetector(modelArmorObj(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	env := modelArmorEnv(t, srv)
	found, err := d.detect(context.Background(), env, StageInput, modelArmorContent)
	if err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]Finding{}
	for _, f := range found {
		byLabel[f.Label] = f
	}
	if f := byLabel[SlotPromptInjection]; !f.Whole || f.Score != 1 {
		t.Errorf("prompt injection: %+v", f)
	}
	if f := byLabel["hate"]; !f.Whole || f.Score != 0.66 {
		t.Errorf("hate: %+v", f)
	}
	if f := byLabel["pii:email_address"]; f.Whole || f.Score != 0.7 || modelArmorContent[f.Start:f.End] != "joe@example.co" {
		t.Errorf("email: %+v", f)
	}
	if f := byLabel["pii:phone_number"]; f.Whole || modelArmorContent[f.Start:f.End] != "555-0100" {
		t.Errorf("phone (code points): %+v", f)
	}
	if f := byLabel["malicious_uri"]; f.Whole || modelArmorContent[f.Start:f.End] != "https://bad.example" {
		t.Errorf("uri: %+v", f)
	}
	if _, ok := byLabel["csam"]; ok {
		t.Error("an unmatched csam result was mapped")
	}
	if len(found) != 5 {
		t.Errorf("findings = %+v", found)
	}
	if m.paths[0] != "/v1/"+modelArmorTemplateName+":sanitizeUserPrompt" || m.bodies[0].UserPromptData == nil ||
		m.bodies[0].UserPromptData.Text != modelArmorContent || m.bodies[0].ModelResponseData != nil {
		t.Fatalf("request %s %+v", m.paths[0], m.bodies[0])
	}
	if m.auth[0] != "Bearer sa-token" {
		t.Fatalf("auth = %q", m.auth[0])
	}
	// A second call reuses the token.
	if _, err := d.detect(context.Background(), env, StageInput, "x"); err != nil {
		t.Fatal(err)
	}
	if m.tokenCalls != 1 {
		t.Fatalf("token exchanges = %d", m.tokenCalls)
	}
}

func TestModelArmorByteRangeIsUsed(t *testing.T) {
	// The email finding has a right byteRange and a wrong codepointRange. The
	// adapter must use the byte range.
	var resp ModelArmorResponse
	if err := json.Unmarshal([]byte(modelArmorMatch), &resp); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, f := range modelArmorFindings(&resp, modelArmorContent) {
		if f.Label == "pii:email_address" {
			seen = true
			if got := modelArmorContent[f.Start:f.End]; got != "joe@example.co" {
				t.Fatalf("email span = %q", got)
			}
		}
	}
	if !seen {
		t.Fatal("no email finding")
	}
}

func TestModelArmorOutputStageAndKeyless(t *testing.T) {
	m := &modelArmorMock{response: `{"sanitizationResult":{"filterMatchState":"NO_MATCH_FOUND","invocationResult":"SUCCESS","filterResults":{}}}`}
	srv := cloudServer(t, m.handler(t))
	obj := modelArmorObj(srv.URL)
	obj["auth_mode"] = "workload_identity"
	delete(obj, "credentials_secret")
	env := cloudEnv(srv)
	env.GoogleAccessToken = func(context.Context) (string, error) { return "wi-token", nil }
	p, col := cloudPipeline(t, StageOutput, env, obj)
	if _, blocked := p.Check(context.Background(), "answer"); blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	if col.Verdicts()[0].Outcome != OutcomePass {
		t.Fatalf("verdict = %+v", col.Verdicts())
	}
	if m.paths[0] != "/v1/"+modelArmorTemplateName+":sanitizeModelResponse" || m.bodies[0].ModelResponseData.Text != "answer" {
		t.Fatalf("request %s %+v", m.paths[0], m.bodies[0])
	}
	if m.auth[0] != "Bearer wi-token" || m.tokenCalls != 0 {
		t.Fatalf("auth = %q, token exchanges %d", m.auth[0], m.tokenCalls)
	}
	// Without an armed workload identity the keyless detector fails.
	env.GoogleAccessToken = nil
	d, _ := newModelArmorDetector(obj)
	if _, err := d.detect(context.Background(), env, StageOutput, "answer"); err == nil {
		t.Fatal("a keyless detector ran without workload identity")
	}
}

func TestModelArmorPartialInvocationIsAnError(t *testing.T) {
	m := &modelArmorMock{response: `{"sanitizationResult":{"filterMatchState":"NO_MATCH_FOUND","invocationResult":"PARTIAL","filterResults":{}}}`}
	srv := cloudServer(t, m.handler(t))
	d, _ := newModelArmorDetector(modelArmorObj(srv.URL))
	if _, err := d.detect(context.Background(), modelArmorEnv(t, srv), StageInput, "x"); err == nil {
		t.Fatal("a partial invocation passed as clean")
	}
}

func TestModelArmorFailuresFailTheRow(t *testing.T) {
	cases := map[string]func(m *modelArmorMock, env *Env){
		"403":            func(m *modelArmorMock, _ *Env) { m.status = http.StatusForbidden },
		"token refused":  func(m *modelArmorMock, _ *Env) { m.tokenStatus = http.StatusBadRequest },
		"egress refused": func(_ *modelArmorMock, env *Env) { env.EgressAllows = func(string) bool { return false } },
		"not a key doc": func(_ *modelArmorMock, env *Env) {
			env.ResolveSecret = func(context.Context, string) (string, error) { return `{"type":"authorized_user"}`, nil }
		},
		"secret missing": func(_ *modelArmorMock, env *Env) { env.ResolveSecret = nil },
		"token uri http": func(_ *modelArmorMock, env *Env) {
			env.ResolveSecret = func(context.Context, string) (string, error) {
				return `{"type":"service_account","token_uri":"http://x"}`, nil
			}
		},
	}
	for name, mutate := range cases {
		m := &modelArmorMock{response: modelArmorMatch}
		srv := cloudServer(t, m.handler(t))
		env := modelArmorEnv(t, srv)
		mutate(m, &env)
		obj := modelArmorObj(srv.URL)
		obj["action"] = "block"
		p, _ := cloudPipeline(t, StageInput, env, obj)
		_, blocked := p.Check(context.Background(), "x")
		if blocked == nil || blocked.Outcome != OutcomeErrorClosed {
			t.Errorf("%s: blocked = %+v", name, blocked)
		}
	}
}

func TestModelArmorDefaultEndpointAndBadConfig(t *testing.T) {
	obj := modelArmorObj("")
	delete(obj, "endpoint")
	d, err := newModelArmorDetector(obj)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.(*modelArmorDetector).cfg.Endpoint; got != "https://modelarmor.us-central1.rep.googleapis.com" {
		t.Fatalf("endpoint = %s", got)
	}
	bad := map[string]func(map[string]any){
		"no template":        func(o map[string]any) { delete(o, "template") },
		"location injection": func(o map[string]any) { o["template"] = "projects/p/locations/x.evil.com/templates/t" },
		"bad auth mode":      func(o map[string]any) { o["auth_mode"] = "ambient" },
		"key without secret": func(o map[string]any) { delete(o, "credentials_secret") },
		"raw secret":         func(o map[string]any) { o["credentials_secret"] = `{"type":"service_account"}` },
		"keyless with secret": func(o map[string]any) {
			o["auth_mode"] = "workload_identity"
		},
	}
	for name, mutate := range bad {
		o := modelArmorObj("https://modelarmor.example.com")
		mutate(o)
		if _, err := newModelArmorDetector(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestModelArmorChecksEgressOnEveryTokenExchange: a token exchange after the
// token_uri host left the egress allowlist fails. No cached token source
// keeps the first request's egress decision.
func TestModelArmorChecksEgressOnEveryTokenExchange(t *testing.T) {
	// A token valid for 30 s is below the cache margin: each call exchanges.
	m := &modelArmorMock{response: modelArmorMatch, expiresIn: 30}
	srv := cloudServer(t, m.handler(t))
	env := modelArmorEnv(t, srv)
	d, err := newModelArmorDetector(modelArmorObj(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.detect(context.Background(), env, StageInput, "x"); err != nil || m.tokenCalls != 1 {
		t.Fatalf("first call: %v, token exchanges %d", err, m.tokenCalls)
	}
	allowed := env.EgressAllows
	env.EgressAllows = func(u string) bool { return allowed(u) && !strings.HasSuffix(u, "/token") }
	if _, err := d.detect(context.Background(), env, StageInput, "x"); err == nil || !strings.Contains(err.Error(), "token_uri") {
		t.Fatalf("an exchange ran with the token host off the allowlist: %v", err)
	}
	if m.tokenCalls != 1 {
		t.Fatalf("token exchanges = %d, want 1", m.tokenCalls)
	}
}

// TestModelArmorCachesTokensOfManyKeys: a valid token is reused, also with
// more keys than the old cache of four sources held.
func TestModelArmorCachesTokensOfManyKeys(t *testing.T) {
	m := &modelArmorMock{response: modelArmorMatch}
	srv := cloudServer(t, m.handler(t))
	env := modelArmorEnv(t, srv)
	docs := make([]string, 6)
	for i := range docs {
		docs[i] = serviceAccountKey(t, srv.URL+"/token")
	}
	var current string
	env.ResolveSecret = func(context.Context, string) (string, error) { return current, nil }
	d, err := newModelArmorDetector(modelArmorObj(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for _, doc := range docs {
			current = doc
			if _, err := d.detect(context.Background(), env, StageInput, "x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if m.tokenCalls != len(docs) {
		t.Fatalf("token exchanges = %d, want %d", m.tokenCalls, len(docs))
	}
}

func TestTokenCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newTokenCache(2)
	a, b := c.entry("a"), c.entry("b")
	if c.entry("a") != a {
		t.Fatal("a cached entry was not reused")
	}
	c.entry("c") // b is the least recently used
	if c.entry("a") != a || c.order.Len() != 2 {
		t.Fatal("the recently used entry was evicted")
	}
	if c.entry("b") == b {
		t.Fatal("the least recently used entry was kept")
	}
}

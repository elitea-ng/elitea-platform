package evaluation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// The AI judge against a REAL model, opt-in.
//
// WHY THIS EXISTS. Every other test in this package hands the judge a canned
// string. That proves the parse, the score schema and the normalisation, and it
// proves nothing at all about the one question this feature turns on: does a
// real model, given this prompt, answer in the schema? A judge that reliably
// answers prose scores every case `error`, and a scorecard of errors is
// indistinguishable — from the stub tests — from a feature that works.
//
// HOW TO RUN IT:
//
//	ELITEA_TEST_LLM_BASE_URL=http://192.168.29.60:8000 \
//	ELITEA_TEST_LLM_MODEL=Qwen/Qwen3.5-35B-A3B-FP8 \
//	go test ./internal/api/v2/evaluation/ -run TestAIJudgeAgainstARealModel -v
//
// It SKIPS without those variables, so CI is unaffected. The skip is loud
// (t.Skipf names the variable) rather than a silent pass — a test that reports
// `ok` while measuring nothing is the shape this repository's evidence bar
// warns about by name.
//
// WHAT IT EXERCISES, AND WHY IT IS THE PRODUCTION CLIENT. The subject is
// `predict.GatewayCompleter` — the SAME client /predict_llm, the three AI-draft
// routes and the orchestrator all take — not a second HTTP client written for
// the test. That client posts to `<base>/llm/v1/chat/completions`, because in
// production its base is elitea-llm-gateway, which mounts the OpenAI dialect
// under /llm. A raw vLLM serves `/v1/chat/completions` with no prefix, so the
// test puts a one-line reverse proxy in front of it that strips `/llm`.
//
// A test that dialled vLLM with its own client instead would prove that a model
// can answer a prompt, and nothing about whether THIS service's client can ask
// it.
func TestAIJudgeAgainstARealModel(t *testing.T) {
	baseURL := os.Getenv("ELITEA_TEST_LLM_BASE_URL")
	if baseURL == "" {
		t.Skipf("set ELITEA_TEST_LLM_BASE_URL (and ELITEA_TEST_LLM_MODEL) to score against a real model")
	}
	model := os.Getenv("ELITEA_TEST_LLM_MODEL")
	if model == "" {
		t.Fatalf("ELITEA_TEST_LLM_BASE_URL is set but ELITEA_TEST_LLM_MODEL is not: the gateway does not guess a model, and neither does this test")
	}

	upstream, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("ELITEA_TEST_LLM_BASE_URL is not a URL: %v", err)
	}
	// The /llm prefix strip. See the doc comment: this is the ONLY difference
	// between the production hop and this one.
	proxy := httptest.NewServer(&httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = upstream.Scheme
			request.URL.Host = upstream.Host
			request.Host = upstream.Host
			request.URL.Path = strings.TrimPrefix(request.URL.Path, "/llm")
		},
	})
	defer proxy.Close()

	// No mTLS and no identity secret: a raw vLLM authenticates neither. The
	// signing path is exercised by the gateway's own tests
	// (internal/llmproxy); what is under test here is the prompt.
	judge := NewAIJudge(newRealModelCompleter(proxy.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dimension := SnapshotDimension{
		Name:        "Correctness",
		Description: "Is the answer factually correct and does it answer the question asked?",
		ScaleType:   ScaleOrdinal,
		ScaleMin:    1,
		ScaleMax:    5,
		Polarity:    PolarityHigherBetter,
	}

	// A GOOD answer and a BAD one. Asserting only that the judge answers in
	// schema would pass for a model that always says 3; the pair is what proves
	// the score carries information.
	good, err := judge.Score(ctx, JudgeRequest{
		ProjectID: "1", Model: model, Dimension: dimension,
		Input:  "What is the capital of France?",
		Output: "The capital of France is Paris.",
	})
	if err != nil {
		t.Fatalf("scoring a correct answer failed: %v", err)
	}
	bad, err := judge.Score(ctx, JudgeRequest{
		ProjectID: "1", Model: model, Dimension: dimension,
		Input:  "What is the capital of France?",
		Output: "Bananas are a good source of potassium.",
	})
	if err != nil {
		t.Fatalf("scoring an incorrect answer failed: %v", err)
	}

	t.Logf("real model %s scored the correct answer %v (%q) and the incorrect one %v (%q)",
		model, good.Score, good.Reason, bad.Score, bad.Reason)

	// The score is INSIDE THE SCALE the prompt stated. A model that answered on
	// a 0..100 scale would clamp to 100 for everything, and the clamp would hide
	// it — this is the assertion that catches a prompt the model ignored.
	if good.Score < dimension.ScaleMin || good.Score > dimension.ScaleMax {
		t.Errorf("the correct answer scored %v, outside the stated %v..%v scale",
			good.Score, dimension.ScaleMin, dimension.ScaleMax)
	}
	if bad.Score < dimension.ScaleMin || bad.Score > dimension.ScaleMax {
		t.Errorf("the incorrect answer scored %v, outside the stated %v..%v scale",
			bad.Score, dimension.ScaleMin, dimension.ScaleMax)
	}
	if !(good.Score > bad.Score) {
		t.Errorf("the judge scored a correct answer %v and an incorrect one %v: the score carries no information",
			good.Score, bad.Score)
	}

	// And the normalisation the scorecard renders agrees with the scale.
	normalized, ok := NormalizeScore(good.Score, dimension.ScaleType, dimension.ScaleMin, dimension.ScaleMax, dimension.Polarity)
	if !ok {
		t.Fatalf("the real score %v could not be normalised", good.Score)
	}
	if normalized < 0 || normalized > 100 {
		t.Errorf("normalised score %v is outside 0..100", normalized)
	}
	t.Logf("normalised: %v / 100", normalized)
}

// newRealModelCompleter builds the PRODUCTION completer against a base URL.
//
// It is `predict.NewGatewayCompleter` and nothing else: no client-level
// timeout (the context carries the deadline), no identity secret (a raw vLLM
// authenticates none), and the default transport rather than mTLS.
func newRealModelCompleter(baseURL string) Completer {
	return predict.NewGatewayCompleter(baseURL, http.DefaultTransport, "")
}

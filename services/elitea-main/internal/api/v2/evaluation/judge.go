package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// Completer is the blocking LLM turn the judge and the agent turn both make.
//
// It is `predict.Completer` restated as a local interface rather than imported
// as one, so this package's tests can supply a stub without building a gateway
// client. The production implementation is the SAME GatewayCompleter the
// /predict_llm route and the three AI-draft routes take
// (RouterConfig.PredictCompleter) — there is no second LLM client in this
// service, and adding one would mean a second place that resolves credentials
// and a second egress path to keep SSRF-safe.
type Completer interface {
	Complete(ctx context.Context, req predict.CompletionRequest) (string, error)
}

// ErrJudgeNotConfigured is what a run reports when no LLM plane is composed.
//
// It is an ERROR and not a zero score. A deployment with no LLM_GATEWAY_URL
// cannot evaluate anything, and a run that answered "every case scored 0"
// would be indistinguishable from an agent that is genuinely terrible.
var ErrJudgeNotConfigured = errors.New("evaluation: no LLM plane is composed (LLM_GATEWAY_URL is unset), so no AI dimension can be scored")

// ErrJudgeUnparseable reports a model answer that is not the score schema.
//
// The case is stored with status `error` and the RAW model text in the
// verdict. It is never stored as `ok` with a score of 0: 0 is a real and very
// bad score, so a judge that rambled would drag a headline down with a number
// nobody measured, and the run would look like evidence of a bad agent instead
// of evidence of a bad prompt.
var ErrJudgeUnparseable = errors.New("evaluation: the judge did not answer in the score schema")

// JudgeRequest is one grading call.
type JudgeRequest struct {
	ProjectID string
	UserID    string
	// Model is the agent version's own model. The judge runs on the SAME model
	// the agent runs on, deliberately: this slice has no separate judge-model
	// setting (that belongs to the suite, which does not exist here), and
	// picking a different model silently would make the score depend on a
	// choice nobody made and nothing records.
	Model     string
	Dimension SnapshotDimension
	// Input, Output and ExpectedOutput are the evidence. ExpectedOutput is a
	// pointer because "no expected answer" and "an expected answer of empty
	// string" are different instructions.
	Input          string
	Output         string
	ExpectedOutput *string
}

// JudgeVerdict is one parsed grading answer.
type JudgeVerdict struct {
	Score  float64
	Reason string
	// Raw is the model's text, kept on both paths. On the failure path it is
	// the only thing that can explain WHY the judge did not answer in schema,
	// and a judge failure is a prompt problem that cannot be diagnosed from the
	// word "error".
	Raw string
}

// Judge grades one (case, dimension) pair.
type Judge interface {
	Score(ctx context.Context, req JudgeRequest) (JudgeVerdict, error)
}

// AIJudge is the `ai` engine: one blocking completion against the dimension's
// rubric, answered as strict JSON.
type AIJudge struct {
	completer Completer
}

// NewAIJudge builds the judge. completer may be nil — that is the
// "LLM_GATEWAY_URL is unset" deployment, and Score then returns
// ErrJudgeNotConfigured. It is NOT a reason to leave the run routes
// unregistered: an unregistered route answers 404, which is indistinguishable
// from a typo'd path, and #126 is the record of what that costs.
func NewAIJudge(completer Completer) *AIJudge {
	return &AIJudge{completer: completer}
}

// judgeSystemPrompt is the instruction half. The dimension's own scale is
// interpolated, so the model is told the range it must answer in rather than
// being left to guess and then be clamped.
const judgeSystemPrompt = `You are an evaluation judge. You grade ONE answer against ONE criterion.

Criterion: %s
Rubric: %s

Answer with a score between %s and %s on the %s scale.

Reply with a single JSON object and nothing else, in exactly this shape:
{"score": <number>, "reason": "<one or two sentences>"}

Do not wrap the JSON in a code fence. Do not add any text before or after it.
If you cannot grade the answer, still reply with the JSON object and explain
why in "reason", using the lowest score on the scale.`

// Score performs one grading call.
func (j *AIJudge) Score(ctx context.Context, req JudgeRequest) (JudgeVerdict, error) {
	if j == nil || j.completer == nil {
		return JudgeVerdict{}, ErrJudgeNotConfigured
	}

	system := fmt.Sprintf(judgeSystemPrompt,
		req.Dimension.Name,
		fallback(req.Dimension.Description, "no rubric was authored; grade on the criterion name alone"),
		formatScaleBound(req.Dimension.ScaleMin),
		formatScaleBound(req.Dimension.ScaleMax),
		req.Dimension.ScaleType,
	)

	var user strings.Builder
	user.WriteString("The question the agent was asked:\n")
	user.WriteString(req.Input)
	user.WriteString("\n\nThe agent's answer:\n")
	user.WriteString(req.Output)
	if req.ExpectedOutput != nil && *req.ExpectedOutput != "" {
		user.WriteString("\n\nThe answer the author expected:\n")
		user.WriteString(*req.ExpectedOutput)
	}

	// Temperature 0. A judge is asked the same question about the same text
	// every time a run is re-executed, and a sampled judge makes a rerun
	// disagree with itself for reasons that have nothing to do with the agent.
	temperature := 0.0
	raw, err := j.completer.Complete(ctx, predict.CompletionRequest{
		ProjectID:   req.ProjectID,
		UserID:      req.UserID,
		Model:       req.Model,
		Temperature: &temperature,
		Messages: []predict.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return JudgeVerdict{}, fmt.Errorf("evaluation: judge call failed: %w", err)
	}

	verdict, ok := parseJudgeAnswer(raw)
	if !ok {
		return JudgeVerdict{Raw: raw}, ErrJudgeUnparseable
	}
	return verdict, nil
}

// parseJudgeAnswer reads `{"score": <number>, "reason": "..."}` out of a model
// answer.
//
// It accepts the object with surrounding text, because a model that has been
// told not to wrap its answer in a code fence still sometimes does, and
// refusing a perfectly good score over a pair of backticks would report a
// prompt-formatting nuisance as an evaluation failure. It does NOT accept prose
// containing a number: the extraction is a JSON parse of the outermost
// brace-delimited span, so "I would give this about 4 out of 5" is
// unparseable — which is the correct answer, because a number scraped out of a
// sentence is a guess about what the model meant.
func parseJudgeAnswer(raw string) (JudgeVerdict, bool) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return JudgeVerdict{}, false
	}

	var parsed struct {
		// A POINTER, so an object that carries no `score` key is rejected
		// rather than read as 0. That is the difference between "the judge did
		// not answer" and "the judge said this is the worst possible answer".
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
		return JudgeVerdict{}, false
	}
	if parsed.Score == nil {
		return JudgeVerdict{}, false
	}
	return JudgeVerdict{Score: *parsed.Score, Reason: parsed.Reason, Raw: raw}, true
}

// formatScaleBound prints a bound without a trailing ".0", so a 1..5 ordinal
// scale is described to the model as "1 and 5" rather than "1.0 and 5.0" — the
// second reads as an invitation to answer 3.7.
func formatScaleBound(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%g", value)
}

func fallback(value, alternative string) string {
	if strings.TrimSpace(value) == "" {
		return alternative
	}
	return value
}

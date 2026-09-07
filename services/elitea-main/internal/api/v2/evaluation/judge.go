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
// IT SCANS FROM THE END FOR THE LAST BALANCED OBJECT, and that is not a
// refinement — it is the fix for a defect a real model found on the first run.
//
// The first version took the span from the first `{` to the last `}`. Every
// stub test passed. Then Qwen3.5-35B was pointed at this prompt and answered
// with three thousand characters of `<think>…</think>` reasoning followed by
// the JSON object, and the reasoning QUOTED THE SCHEMA back — `Output Format:
// {"score": <number>, ...}` — so the widest span started inside the model's own
// notes, swallowed the real object, and failed to parse. Every case would have
// been stored `error` with a perfectly good score sitting at the end of the
// text, and no stub could have shown it.
//
// Reasoning models are the normal case for this workload, not an exotic one, so
// the parse has to survive prose that contains braces. Scanning backwards for
// the last object that PARSES AND CARRIES A NUMERIC SCORE does that without
// knowing anything about `<think>` tags or any other provider's convention.
//
// It still does NOT accept prose containing a number: the extraction is a JSON
// parse, so "I would give this about 4 out of 5" is unparseable — which is the
// correct answer, because a number scraped out of a sentence is a guess about
// what the model meant.
func parseJudgeAnswer(raw string) (JudgeVerdict, bool) {
	for start := strings.LastIndex(raw, "{"); start >= 0; start = strings.LastIndex(raw[:start], "{") {
		end, closed := matchingBrace(raw, start)
		if !closed {
			continue
		}
		verdict, ok := decodeScoreObject(raw[start : end+1])
		if ok {
			verdict.Raw = raw
			return verdict, true
		}
	}
	return JudgeVerdict{}, false
}

// matchingBrace finds the `}` that closes the `{` at start, ignoring braces
// inside JSON strings. A brace inside `"reason"` text is not structure, and a
// depth counter that did not know that would close the object early.
func matchingBrace(raw string, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(raw); index++ {
		char := raw[index]
		switch {
		case escaped:
			escaped = false
		case char == '\\' && inString:
			escaped = true
		case char == '"':
			inString = !inString
		case inString:
			// Nothing: braces inside a string are text.
		case char == '{':
			depth++
		case char == '}':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

// decodeScoreObject accepts one candidate object.
func decodeScoreObject(candidate string) (JudgeVerdict, bool) {
	var parsed struct {
		// A POINTER, so an object that carries no `score` key is rejected
		// rather than read as 0. That is the difference between "the judge did
		// not answer" and "the judge said this is the worst possible answer" —
		// and it is also what makes the backwards scan safe: the model's own
		// notes contain `{"score": <number>, ...}` with a NON-NUMERIC
		// placeholder, which fails to decode and is skipped rather than being
		// read as a score of 0.
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
		return JudgeVerdict{}, false
	}
	if parsed.Score == nil {
		return JudgeVerdict{}, false
	}
	return JudgeVerdict{Score: *parsed.Score, Reason: parsed.Reason}, true
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

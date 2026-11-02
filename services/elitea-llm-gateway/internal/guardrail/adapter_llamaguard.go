package guardrail

// The llama_guard adapter (ADR-0043 decision 1). It sends the content to a
// Llama Guard class model (Llama Guard 3 or 4) that the customer serves
// behind an OpenAI-compatible chat completions endpoint: vLLM, llama.cpp
// (llama-server) or Ollama.
//
// # The request
//
// The adapter sends plain chat messages and lets the model's chat template
// build the Llama Guard prompt (the category list and the instruction). vLLM,
// llama.cpp and Ollama all apply the template of the served model. The
// template classifies the LAST turn:
//
//   - input stage: one user message with the content;
//   - output stage: a user message with a fixed placeholder, then an
//     assistant message with the content. The detector reads the answer
//     only, so the user turn of the conversation is not available here.
//
// temperature is 0 and max_tokens is small: the answer is one or two short
// lines.
//
// # The answer
//
// The model answers `safe`, or `unsafe` and, on the next line, the violated
// category codes separated by commas (`unsafe\nS1,S10`). The adapter maps
// each code to a label (llamaGuardLabels). An unknown code becomes its lower
// case form (`s15`). `unsafe` with no code is the label `unsafe`. Every
// finding covers the whole content and has score 1: the model gives no
// calibrated score.
//
// `categories` limits the codes that count. A code outside the list is not a
// finding. The model still evaluates every category of its template.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TypeLlamaGuard is the detector type of the Llama Guard adapter.
const TypeLlamaGuard = "llama_guard"

// LlamaGuardOutputUserTurn is the user turn the adapter sends before the
// answer on the output stage.
const LlamaGuardOutputUserTurn = "(The user turn is not part of this check.)"

// maxLlamaGuardModelBytes bounds the model name.
const maxLlamaGuardModelBytes = 256

// llamaGuardMaxTokens bounds the answer: "unsafe" and up to 14 codes.
const llamaGuardMaxTokens = 32

// llamaGuardLabels maps the hazard category codes of the Llama Guard 3 and
// Llama Guard 4 model cards (the MLCommons hazard taxonomy) to labels. Both
// models use the same 14 codes.
var llamaGuardLabels = map[string]string{
	"S1":  "violent_crimes",
	"S2":  "non_violent_crimes",
	"S3":  "sex_related_crimes",
	"S4":  "child_sexual_exploitation",
	"S5":  "defamation",
	"S6":  "specialized_advice",
	"S7":  "privacy",
	"S8":  "intellectual_property",
	"S9":  "indiscriminate_weapons",
	"S10": "hate",
	"S11": "suicide_self_harm",
	"S12": "sexual_content",
	"S13": "elections",
	"S14": "code_interpreter_abuse",
}

// LlamaGuardLabel returns the label of a category code.
func LlamaGuardLabel(code string) string {
	if l, ok := llamaGuardLabels[strings.ToUpper(code)]; ok {
		return l
	}
	return strings.ToLower(code)
}

var llamaGuardCodePattern = regexp.MustCompile(`^S[0-9]{1,2}$`)

type llamaGuardDetector struct {
	ep         selfHostedEndpoint
	model      string
	categories []string
}

func newLlamaGuardDetector(obj map[string]any) (detectorImpl, error) {
	ep, err := parseSelfHostedEndpoint(obj, "Authorization", "Bearer ")
	if err != nil {
		return nil, err
	}
	d := &llamaGuardDetector{ep: ep}
	d.model, _ = obj["model"].(string)
	d.model = strings.TrimSpace(d.model)
	if d.model == "" || len(d.model) > maxLlamaGuardModelBytes {
		return nil, fmt.Errorf("model must name the served Llama Guard model (1 to %d bytes)", maxLlamaGuardModelBytes)
	}
	cats, err := stringList(obj, "categories")
	if err != nil {
		return nil, err
	}
	if len(cats) > len(llamaGuardLabels) {
		return nil, fmt.Errorf("categories lists %d entries; the limit is %d", len(cats), len(llamaGuardLabels))
	}
	for _, c := range cats {
		if !llamaGuardCodePattern.MatchString(c) {
			return nil, fmt.Errorf("categories entry %q must be a category code such as S1", c)
		}
	}
	d.categories = cats
	return d, nil
}

type llamaGuardMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llamaGuardRequest struct {
	Model       string              `json:"model"`
	Messages    []llamaGuardMessage `json:"messages"`
	Temperature float64             `json:"temperature"`
	MaxTokens   int                 `json:"max_tokens"`
	Stream      bool                `json:"stream"`
}

type llamaGuardResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// llamaGuardMessages builds the conversation the chat template classifies.
func llamaGuardMessages(stage, content string) []llamaGuardMessage {
	if stage == StageOutput {
		return []llamaGuardMessage{
			{Role: "user", Content: LlamaGuardOutputUserTurn},
			{Role: "assistant", Content: content},
		}
	}
	return []llamaGuardMessage{{Role: "user", Content: content}}
}

func (d *llamaGuardDetector) detect(ctx context.Context, env Env, stage, content string) ([]Finding, error) {
	raw, err := d.ep.post(ctx, env, llamaGuardRequest{
		Model: d.model, Messages: llamaGuardMessages(stage, content),
		MaxTokens: llamaGuardMaxTokens,
	})
	if err != nil {
		return nil, err
	}
	var parsed llamaGuardResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == nil {
		return nil, fmt.Errorf("%w: response is not a chat completion", errAdapter)
	}
	codes, unsafe, err := parseLlamaGuardAnswer(*parsed.Choices[0].Message.Content)
	if err != nil {
		return nil, err
	}
	if !unsafe {
		return nil, nil
	}
	if len(codes) == 0 {
		if len(d.categories) > 0 {
			// The model named no category, so no listed category is violated.
			return nil, nil
		}
		return []Finding{{Whole: true, Label: "unsafe", Score: 1}}, nil
	}
	var out []Finding
	for _, code := range codes {
		if len(d.categories) > 0 && !containsString(d.categories, code) {
			continue
		}
		out = append(out, Finding{Whole: true, Label: LlamaGuardLabel(code), Score: 1})
	}
	return out, nil
}

var errLlamaGuardAnswer = errors.New("the answer is not safe or unsafe")

// parseLlamaGuardAnswer reads `safe` or `unsafe` and the category codes. It
// accepts the codes on the next line or on the same line, separated by
// commas or spaces.
func parseLlamaGuardAnswer(answer string) (codes []string, unsafe bool, err error) {
	fields := strings.FieldsFunc(strings.TrimSpace(answer), func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	if len(fields) == 0 {
		return nil, false, fmt.Errorf("%w: %v", errAdapter, errLlamaGuardAnswer)
	}
	switch strings.ToLower(fields[0]) {
	case "safe":
		return nil, false, nil
	case "unsafe":
	default:
		return nil, false, fmt.Errorf("%w: %v", errAdapter, errLlamaGuardAnswer)
	}
	seen := map[string]bool{}
	for _, f := range fields[1:] {
		code := strings.ToUpper(f)
		if !llamaGuardCodePattern.MatchString(code) {
			// A model can add text after the codes. Only codes count.
			continue
		}
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes, true, nil
}

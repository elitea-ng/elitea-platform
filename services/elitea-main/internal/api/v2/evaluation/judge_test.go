package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// stubCompleter answers a canned string, or an error, and records what it was
// asked. It is the only LLM in this package's unit tests: the production
// implementation is the gateway client /predict_llm takes, and exercising it
// here would test the gateway rather than the judge.
type stubCompleter struct {
	answer   string
	err      error
	requests []predict.CompletionRequest
}

func (s *stubCompleter) Complete(_ context.Context, req predict.CompletionRequest) (string, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return "", s.err
	}
	return s.answer, nil
}

func aiDimension() SnapshotDimension {
	return SnapshotDimension{
		Name:        "Helpfulness",
		Description: "Does the answer solve the user's problem?",
		ScaleType:   ScaleOrdinal,
		ScaleMin:    1,
		ScaleMax:    5,
		Polarity:    PolarityHigherBetter,
	}
}

func TestAIJudgeParsesTheScoreSchema(t *testing.T) {
	t.Parallel()

	completer := &stubCompleter{answer: `{"score": 4, "reason": "it answered the question"}`}
	verdict, err := NewAIJudge(completer).Score(context.Background(), JudgeRequest{
		ProjectID: "7", UserID: "42", Model: "gpt-4o",
		Dimension: aiDimension(), Input: "hello", Output: "hi",
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if verdict.Score != 4 {
		t.Errorf("score = %v, want 4", verdict.Score)
	}
	if verdict.Reason != "it answered the question" {
		t.Errorf("reason = %q", verdict.Reason)
	}
}

// A code fence is a formatting nuisance, not an evaluation failure. Refusing it
// would report a perfectly good score as unparseable and mark the case errored.
func TestAIJudgeAcceptsAFencedAnswer(t *testing.T) {
	t.Parallel()

	completer := &stubCompleter{answer: "Here you go:\n```json\n{\"score\": 2.5, \"reason\": \"partly\"}\n```\n"}
	verdict, err := NewAIJudge(completer).Score(context.Background(), JudgeRequest{
		ProjectID: "7", Dimension: aiDimension(), Input: "q", Output: "a",
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if verdict.Score != 2.5 {
		t.Errorf("score = %v, want 2.5", verdict.Score)
	}
}

// THE CENTRAL RULE OF THIS SLICE. A judge that answers prose produces an ERROR,
// not a score of zero. Zero is a real and very bad score, so a run whose judge
// rambled would look like evidence of a bad agent instead of a bad prompt.
func TestAIJudgeReportsProseAsAnErrorAndNeverAsZero(t *testing.T) {
	t.Parallel()

	for _, answer := range []string{
		"I would give this about 4 out of 5, it was quite good.",
		"",
		"{",
		`{"reason": "I forgot the score"}`,
		`{"score": null, "reason": "unknown"}`,
	} {
		verdict, err := NewAIJudge(&stubCompleter{answer: answer}).Score(context.Background(), JudgeRequest{
			ProjectID: "7", Dimension: aiDimension(), Input: "q", Output: "a",
		})
		if !errors.Is(err, ErrJudgeUnparseable) {
			t.Errorf("answer %q: err = %v, want ErrJudgeUnparseable", answer, err)
		}
		if verdict.Score != 0 {
			continue
		}
		// The zero here is the ZERO VALUE of an unused struct, and the caller
		// must never read it: the error is the answer. What the caller DOES
		// need is the raw text, so a prompt problem can be diagnosed.
		if answer != "" && verdict.Raw != answer {
			t.Errorf("answer %q: raw = %q, want the model's own text", answer, verdict.Raw)
		}
	}
}

// A missing LLM plane is reported as a NAMED error, not as a run of zeros. A
// deployment with no LLM_GATEWAY_URL cannot evaluate anything, and a scorecard
// of zeros would be indistinguishable from a genuinely terrible agent.
func TestAIJudgeWithoutACompleterReportsTheMissingPlane(t *testing.T) {
	t.Parallel()

	if _, err := NewAIJudge(nil).Score(context.Background(), JudgeRequest{
		ProjectID: "7", Dimension: aiDimension(),
	}); !errors.Is(err, ErrJudgeNotConfigured) {
		t.Fatalf("err = %v, want ErrJudgeNotConfigured", err)
	}
	// A nil *AIJudge too — the composition root may hand one over when no
	// judge was built, and a nil-receiver panic would take the whole worker
	// down rather than failing one run.
	var absent *AIJudge
	if _, err := absent.Score(context.Background(), JudgeRequest{}); !errors.Is(err, ErrJudgeNotConfigured) {
		t.Fatalf("nil judge: err = %v, want ErrJudgeNotConfigured", err)
	}
}

// A transport failure is an error and NOT a zero score, for the same reason.
func TestAIJudgeReportsATransportFailure(t *testing.T) {
	t.Parallel()

	_, err := NewAIJudge(&stubCompleter{err: errors.New("gateway said 429")}).
		Score(context.Background(), JudgeRequest{ProjectID: "7", Dimension: aiDimension()})
	if err == nil {
		t.Fatal("a failed gateway call produced no error")
	}
	if errors.Is(err, ErrJudgeUnparseable) {
		t.Error("a transport failure was reported as an unparseable answer")
	}
}

// The judge runs at temperature 0 and carries the identity of the person who
// started the run. A sampled judge makes a rerun disagree with itself for
// reasons that have nothing to do with the agent, and an unsigned call bills
// the project to nobody.
func TestAIJudgeSendsADeterministicRequestWithTheCallerIdentity(t *testing.T) {
	t.Parallel()

	completer := &stubCompleter{answer: `{"score": 5}`}
	if _, err := NewAIJudge(completer).Score(context.Background(), JudgeRequest{
		ProjectID: "7", UserID: "42", Model: "qwen", Dimension: aiDimension(),
		Input: "what is 2+2", Output: "4",
	}); err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(completer.requests) != 1 {
		t.Fatalf("the judge made %d calls, want 1", len(completer.requests))
	}
	request := completer.requests[0]
	if request.Temperature == nil || *request.Temperature != 0 {
		t.Errorf("temperature = %v, want an explicit 0", request.Temperature)
	}
	if request.ProjectID != "7" || request.UserID != "42" {
		t.Errorf("identity = project %q user %q, want 7 / 42", request.ProjectID, request.UserID)
	}
	if request.Model != "qwen" {
		t.Errorf("model = %q, want the agent version's own model", request.Model)
	}
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want a system prompt and a user turn", request.Messages)
	}
	// The SCALE must be in the prompt. Without it the model answers on
	// whatever scale it assumes, the clamp hides the mismatch, and every score
	// lands at 100.
	if !strings.Contains(request.Messages[0].Content, "between 1 and 5") {
		t.Errorf("the judge prompt does not state the dimension's scale:\n%s", request.Messages[0].Content)
	}
	// The RUBRIC must be in the prompt: it is what the dimension means.
	if !strings.Contains(request.Messages[0].Content, "solve the user's problem") {
		t.Error("the judge prompt does not carry the dimension's rubric")
	}
}

// The expected output reaches the judge only when the author stated one. A
// judge told to compare against an empty string marks every non-empty answer
// wrong, which is why the field is a pointer all the way down.
func TestAIJudgeOmitsAnUnstatedExpectedOutput(t *testing.T) {
	t.Parallel()

	completer := &stubCompleter{answer: `{"score": 3}`}
	if _, err := NewAIJudge(completer).Score(context.Background(), JudgeRequest{
		ProjectID: "7", Dimension: aiDimension(), Input: "q", Output: "a",
	}); err != nil {
		t.Fatalf("Score: %v", err)
	}
	if strings.Contains(completer.requests[0].Messages[1].Content, "expected") {
		t.Error("the judge was given an expected answer the author never stated")
	}

	expected := "the right answer"
	completer.requests = nil
	if _, err := NewAIJudge(completer).Score(context.Background(), JudgeRequest{
		ProjectID: "7", Dimension: aiDimension(), Input: "q", Output: "a",
		ExpectedOutput: &expected,
	}); err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !strings.Contains(completer.requests[0].Messages[1].Content, expected) {
		t.Error("the stated expected answer did not reach the judge")
	}
}

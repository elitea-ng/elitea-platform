package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func enhanceRouter(repo RunRepository, completer Completer) http.Handler {
	handler := NewEnhanceHandler(repo, completer)
	r := chi.NewRouter()
	r.Post("/eval_run_enhance/prompt_lib/{projectID}/{runID}", handler.Enhance)
	return r
}

func boolPtr(value bool) *bool        { return &value }
func floatPtr(value float64) *float64 { return &value }

// finishedRunWithMisses seeds a finished run whose case 11 missed its target on
// dimension 3 and whose case 12 met it.
func finishedRunWithMisses(t *testing.T) *fakeRunRepo {
	t.Helper()
	repo, run := twoCaseRun()
	expected := "Go is a language"
	repo.cases[0].ExpectedOutput = &expected
	run.Status = RunStatusFinished
	target := 4.0
	run.Snapshot.Bindings[0].Target = &target
	run.Snapshot.Bindings[0].TargetOperator = ">="
	run.Snapshot.Dimensions["3"] = SnapshotDimension{
		Name: "Helpfulness", Description: "Does it help?", ScaleType: ScaleOrdinal,
		ScaleMin: 1, ScaleMax: 5, Polarity: PolarityHigherBetter,
	}
	repo.seedRun(run)
	save := func(caseID string, score, normalized float64, met bool, reason string) {
		_ = repo.SaveResult(context.Background(), "1", RunResult{
			RunID: "1", DatasetCaseID: caseID, DimensionID: "3", Status: ResultStatusOK,
			NativeScore: &score, NormalizedScore: &normalized, TargetMet: boolPtr(met),
			Verdict:  map[string]any{"score": score, "reason": reason},
			Evidence: map[string]any{"input": "q", "output": "the agent said " + caseID},
		})
	}
	save("11", 2, 25, false, "too vague")
	save("12", 5, 100, true, "fine")
	return repo
}

const goodAnswer = `{"summary":"The agent is vague.",
"instruction_patch":{"rationale":"be concrete","instructions":"be helpful and concrete"},
"dimension_changes":[
 {"dimension_id":"3","description":"Does it give a concrete answer?","default_target":3,"default_target_operator":">=","rationale":"rubric was loose"},
 {"dimension_id":"99","description":"x","rationale":"unknown dimension"}],
"dataset_changes":[
 {"case_id":"11","expected_output":"Go is a compiled language","rationale":"too narrow"},
 {"case_id":"12","expected_output":"met its target, not in the evidence","rationale":"x"}]}`

func TestEnhanceAnswersAValidatedProposalFromTheMissedCases(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	completer := &stubCompleter{answer: "<think>{\"summary\": <text>}</think>\n" + goodAnswer}
	response := call(t, enhanceRouter(repo, completer), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}
	var proposal EnhanceProposal
	if err := json.Unmarshal(response.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.InstructionPatch == nil || proposal.InstructionPatch.Instructions != "be helpful and concrete" {
		t.Errorf("patch = %+v", proposal.InstructionPatch)
	}
	// A dimension outside the evidence is dropped, not repaired.
	if len(proposal.DimensionChanges) != 1 || proposal.DimensionChanges[0].DimensionID != "3" ||
		proposal.DimensionChanges[0].DefaultTarget == nil || *proposal.DimensionChanges[0].DefaultTarget != 3 {
		t.Errorf("dimension changes = %+v", proposal.DimensionChanges)
	}
	// Case 12 met its target: a change to it is not supported by the evidence.
	if len(proposal.DatasetChanges) != 1 || proposal.DatasetChanges[0].CaseID != "11" ||
		proposal.DatasetChanges[0].DatasetID != "5" ||
		proposal.DatasetChanges[0].CurrentExpectedOutput == nil {
		t.Errorf("dataset changes = %+v", proposal.DatasetChanges)
	}
	if proposal.MissedRows != 1 || proposal.AnalysedRows != 1 {
		t.Errorf("rows = %d/%d, want 1/1", proposal.AnalysedRows, proposal.MissedRows)
	}

	// The model saw the PINNED instructions, the missed case, the judge's
	// rationale, and NOT the case that met its target.
	if len(completer.requests) != 1 {
		t.Fatalf("model called %d times, want once", len(completer.requests))
	}
	request := completer.requests[0]
	prompt := request.Messages[1].Content
	for _, want := range []string{"be helpful", "too vague", "the agent said 11", "Go is a language", "Does it help?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "the agent said 12") {
		t.Error("a case that met its target was sent as a miss")
	}
	if request.Model != "qwen" || request.AttributionID != "" {
		t.Errorf("model %q attribution %q: want the version's model and no run attribution", request.Model, request.AttributionID)
	}
}

func TestEnhanceReadsTheFrozenAgentNotTheVersionRow(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	run := repo.run("1")
	run.Snapshot.Agent = &SnapshotAgent{Instructions: "frozen at gate start", ModelName: "frozen-model"}
	repo.seedRun(run)
	completer := &stubCompleter{answer: goodAnswer}
	if r := call(t, enhanceRouter(repo, completer), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", r.Code, r.Body.String())
	}
	if !strings.Contains(completer.requests[0].Messages[1].Content, "frozen at gate start") || completer.requests[0].Model != "frozen-model" {
		t.Error("the proposal was built from the version row, not the run's frozen agent")
	}
}

func TestEnhanceRefusals(t *testing.T) {
	t.Parallel()

	t.Run("run not finished", func(t *testing.T) {
		t.Parallel()
		repo := finishedRunWithMisses(t)
		run := repo.run("1")
		run.Status = RunStatusRunning
		repo.seedRun(run)
		if r := call(t, enhanceRouter(repo, &stubCompleter{answer: goodAnswer}), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusConflict {
			t.Errorf("code = %d, want 409", r.Code)
		}
	})
	t.Run("nothing missed", func(t *testing.T) {
		t.Parallel()
		repo, run := twoCaseRun()
		run.Status = RunStatusFinished
		repo.seedRun(run)
		completer := &stubCompleter{answer: goodAnswer}
		if r := call(t, enhanceRouter(repo, completer), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422", r.Code)
		}
		if len(completer.requests) != 0 {
			t.Error("the model was called with nothing to analyse")
		}
	})
	t.Run("no gateway", func(t *testing.T) {
		t.Parallel()
		if r := call(t, enhanceRouter(finishedRunWithMisses(t), nil), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", r.Code)
		}
	})
	t.Run("gateway failure", func(t *testing.T) {
		t.Parallel()
		if r := call(t, enhanceRouter(finishedRunWithMisses(t), &stubCompleter{err: errors.New("boom")}), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusBadGateway {
			t.Errorf("code = %d, want 502", r.Code)
		}
	})
	t.Run("unusable answer", func(t *testing.T) {
		t.Parallel()
		if r := call(t, enhanceRouter(finishedRunWithMisses(t), &stubCompleter{answer: "I would change the prompt."}), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", ""); r.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422", r.Code)
		}
	})
}

func TestValidateProposalDropsNoOpsAndBadOperators(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	run := repo.run("1")
	rows := missedRows(run, repo.storedResults(), repo.cases)
	same := "be helpful"
	raw, ok := parseEnhanceAnswer(`{"summary":"s","instruction_patch":{"rationale":"r","instructions":"` + same + `"},
	"dimension_changes":[{"dimension_id":"3","default_target":9,"default_target_operator":"~=","rationale":"bad operator, no rubric"}],
	"dataset_changes":[{"case_id":"11","expected_output":"Go is a language","rationale":"same as stored"}]}`)
	if !ok {
		t.Fatal("did not parse")
	}
	proposal := validateProposal(raw, "be helpful", run, rows)
	if proposal.InstructionPatch != nil || len(proposal.DimensionChanges) != 0 || len(proposal.DatasetChanges) != 0 {
		t.Errorf("no-op fixes survived: %+v", proposal)
	}
}

func TestEnhanceCapsTheEvidenceAndKeepsTheWorstScores(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	run := repo.run("1")
	var results []RunResult
	var cases []DatasetCase
	for index := 0; index < MaxEnhanceMissedRows+5; index++ {
		id := string(rune('a'+index%26)) + string(rune('a'+index/26))
		cases = append(cases, DatasetCase{ID: id, Input: "q"})
		score := float64(index)
		results = append(results, RunResult{DatasetCaseID: id, DimensionID: "3", Status: ResultStatusOK,
			NormalizedScore: &score, TargetMet: boolPtr(false)})
	}
	rows := missedRows(run, results, cases)
	if len(rows) != MaxEnhanceMissedRows+5 || *rows[0].Result.NormalizedScore != 0 {
		t.Fatalf("rows = %d, first score %v: want all of them, worst first", len(rows), rows[0].Result.NormalizedScore)
	}
}

func TestEnhanceDoesNotRequestOrAcceptAPatchForOversizeInstructions(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	repo.version.Instructions = strings.Repeat("x", MaxEnhanceInstructionRunes+1)
	completer := &stubCompleter{answer: goodAnswer}
	response := call(t, enhanceRouter(repo, completer), http.MethodPost, "/eval_run_enhance/prompt_lib/1/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}
	var proposal EnhanceProposal
	if err := json.Unmarshal(response.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	// The model said yes to a patch; the server does not take it, and says why.
	if proposal.InstructionPatch != nil || !strings.Contains(proposal.InstructionPatchUnavailable, "longer than") {
		t.Errorf("patch %+v unavailable %q", proposal.InstructionPatch, proposal.InstructionPatchUnavailable)
	}
	if !strings.Contains(completer.requests[0].Messages[0].Content, "DO NOT propose an instruction_patch") {
		t.Error("the model was still asked for a patch")
	}
	// The other two kinds are unaffected.
	if len(proposal.DimensionChanges) != 1 || len(proposal.DatasetChanges) != 1 {
		t.Errorf("other fixes lost: %+v", proposal)
	}
}

func TestEnhanceTargetMustLieOnTheScaleAndNamesTheRunTarget(t *testing.T) {
	t.Parallel()

	repo := finishedRunWithMisses(t)
	run := repo.run("1")
	rows := missedRows(run, repo.storedResults(), repo.cases)
	raw, _ := parseEnhanceAnswer(`{"summary":"s","dimension_changes":[
	 {"dimension_id":"3","default_target":9,"default_target_operator":">=","description":"new rubric","rationale":"off the 1..5 scale"}]}`)
	proposal := validateProposal(raw, "be helpful", run, rows)
	if len(proposal.DimensionChanges) != 1 {
		t.Fatalf("changes = %+v", proposal.DimensionChanges)
	}
	change := proposal.DimensionChanges[0]
	if change.DefaultTarget != nil {
		t.Errorf("a target of 9 on a 1..5 scale was accepted: %v", *change.DefaultTarget)
	}
	if change.RunTarget == nil || *change.RunTarget != 4 || change.RunTargetOperator != ">=" {
		t.Errorf("run target = %v %q, want the binding's >= 4", change.RunTarget, change.RunTargetOperator)
	}
}

// The latency of the agent turn is recorded on every result row of the case.
func TestOrchestratorRecordsAgentLatencyAndTimeouts(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 3}}}
	NewOrchestrator(repo, judge, &stubCompleter{answer: "a"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})
	for _, result := range repo.storedResults() {
		if _, ok := result.Evidence["agent_latency_ms"].(int64); !ok {
			t.Errorf("case %s: no agent_latency_ms in %v", result.DatasetCaseID, result.Evidence)
		}
		if _, timedOut := result.Evidence["agent_timed_out"]; timedOut {
			t.Errorf("case %s: flagged as timed out", result.DatasetCaseID)
		}
	}

	evidence := agentTurnEvidence(1500*time.Millisecond, context.DeadlineExceeded)
	if evidence["agent_latency_ms"] != int64(1500) || evidence["agent_timed_out"] != true {
		t.Errorf("evidence = %v", evidence)
	}
	if _, flagged := agentTurnEvidence(time.Second, errors.New("502"))["agent_timed_out"]; flagged {
		t.Error("a non-timeout failure was flagged as a timeout")
	}
	// Typed signals: a gateway 504 or 408, and a gRPC deadline.
	for name, err := range map[string]error{
		"504":  fmt.Errorf("predict: %w", &predict.GatewayStatusError{Status: 504}),
		"408":  &predict.GatewayStatusError{Status: 408},
		"grpc": status.Error(codes.DeadlineExceeded, "slow"),
	} {
		if agentTurnEvidence(time.Second, err)["agent_timed_out"] != true {
			t.Errorf("%s was not flagged as a timeout", name)
		}
	}
	if _, flagged := agentTurnEvidence(time.Second, &predict.GatewayStatusError{Status: 502})["agent_timed_out"]; flagged {
		t.Error("a 502 was flagged as a timeout")
	}
}

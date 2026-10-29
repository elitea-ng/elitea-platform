package releasegate

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// Validate checks a suite config before it is stored. It answers an error
// that wraps ErrInvalidConfig with the reason.
func (s SuiteConfig) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
	}
	if s.ApplicationID <= 0 || s.DatasetID <= 0 {
		return invalid("the agent and the dataset are required")
	}
	if len(s.Criteria) == 0 {
		return invalid("a gate suite needs at least one criterion")
	}
	if len(s.Criteria) > MaxCriteria {
		return invalid("a gate suite has at most %d criteria", MaxCriteria)
	}
	if s.ReevaluateEveryDays != nil &&
		(*s.ReevaluateEveryDays < 1 || *s.ReevaluateEveryDays > MaxReevaluateEveryDays) {
		return invalid("reevaluate_every_days must be between 1 and %d", MaxReevaluateEveryDays)
	}
	seen := map[string]bool{}
	for _, criterion := range s.Criteria {
		if id, err := strconv.Atoi(criterion.DimensionID); err != nil || id <= 0 {
			return invalid("dimension_id must be a positive integer")
		}
		if seen[criterion.DimensionID] {
			return invalid("dimension %s has more than one criterion", criterion.DimensionID)
		}
		seen[criterion.DimensionID] = true
		switch criterion.Kind {
		case CriterionThreshold:
			if criterion.MinScore == nil || !inRange(*criterion.MinScore) || criterion.MaxDrop != nil {
				return invalid("a threshold criterion needs min_score between 0 and 100, and no max_drop")
			}
		case CriterionNoWorseThan:
			if criterion.MaxDrop == nil || !inRange(*criterion.MaxDrop) || criterion.MinScore != nil {
				return invalid("a no_worse_than criterion needs max_drop between 0 and 100, and no min_score")
			}
		default:
			return invalid("criterion kind must be threshold or no_worse_than")
		}
	}
	return nil
}

func inRange(value float64) bool {
	return !math.IsNaN(value) && value >= 0 && value <= 100
}

// RunEvidence is one terminal evaluation run as the evaluation reads it.
type RunEvidence struct {
	EvalRunID int64
	// Missing is true when the run row no longer exists.
	Missing bool
	Status  string
	Error   string
	Results []evaluation.RunResult
}

// SuiteEvidence is everything Evaluate needs for one suite.
type SuiteEvidence struct {
	Suite SuiteConfig
	// NotRunnable is the reason the suite could not start, or empty.
	NotRunnable string
	// Skipped is a suite that did not run because another suite of the
	// gate could not start. It adds no failure of its own.
	Skipped   bool
	Candidate *RunEvidence
	// Baseline is the run against the current release, or nil when the
	// suite has none: there is no current release, or the suite needs no
	// baseline.
	Baseline *RunEvidence
	// CaseNames maps a case id to its name (the start of its input).
	CaseNames map[string]string
}

// Evaluate decides a gate from the evidence of its suites. It answers the
// state (passed or failed), the outcome per suite and the failures. A gate
// with no suite passes. A run that errored, was cancelled or is missing
// fails the gate: a gate is never passed on evidence it does not have.
func Evaluate(suites []SuiteEvidence) (string, []SuiteOutcome, []GateFailure) {
	outcomes := make([]SuiteOutcome, 0, len(suites))
	failures := []GateFailure{}
	for _, evidence := range suites {
		outcome, suiteFailures := evaluateSuite(evidence)
		outcomes = append(outcomes, outcome)
		failures = append(failures, suiteFailures...)
	}
	if len(failures) > 0 {
		return StateFailed, outcomes, failures
	}
	return StatePassed, outcomes, failures
}

func evaluateSuite(evidence SuiteEvidence) (SuiteOutcome, []GateFailure) {
	suite := evidence.Suite
	outcome := SuiteOutcome{
		DatasetID: suite.DatasetID, Regression: suite.Regression,
		Criteria: make([]CriterionOutcome, 0, len(suite.Criteria)), RegressedCases: []RegressedCase{},
	}
	notEvaluated := func(note string) SuiteOutcome {
		for _, criterion := range suite.Criteria {
			outcome.Criteria = append(outcome.Criteria, CriterionOutcome{
				Criterion: criterion, Status: CriterionNotEvaluated, Note: note,
			})
		}
		return outcome
	}

	if evidence.NotRunnable != "" {
		return notEvaluated("the suite did not run"), []GateFailure{{
			Code: FailureSuiteNotRunnable, DatasetID: suite.DatasetID, Message: evidence.NotRunnable,
		}}
	}
	if evidence.Skipped {
		return notEvaluated("the gate stopped before this suite ran"), nil
	}
	var failures []GateFailure
	if failure, failed := runFailure(suite.DatasetID, RoleCandidate, evidence.Candidate); failed {
		failures = append(failures, failure)
	}
	if evidence.Baseline != nil {
		if failure, failed := runFailure(suite.DatasetID, RoleBaseline, evidence.Baseline); failed {
			failures = append(failures, failure)
		}
	}
	if len(failures) > 0 {
		return notEvaluated("a run of the suite did not finish"), failures
	}

	candidate := summarise(evidence.Candidate.Results)
	var baseline map[string]dimensionSummary
	if evidence.Baseline != nil {
		baseline = summarise(evidence.Baseline.Results)
	}
	for _, criterion := range suite.Criteria {
		result, failure := evaluateCriterion(suite.DatasetID, criterion, candidate, baseline, evidence)
		outcome.Criteria = append(outcome.Criteria, result)
		if failure != nil {
			failures = append(failures, *failure)
		}
	}

	if suite.Regression && evidence.Baseline != nil {
		outcome.RegressedCases = regressedCases(suite, evidence)
		if len(outcome.RegressedCases) > 0 {
			failures = append(failures, GateFailure{
				Code: FailureRegression, DatasetID: suite.DatasetID, EvalRunID: evidence.Candidate.EvalRunID,
				Cases: outcome.RegressedCases,
				Message: fmt.Sprintf("%d case(s) passed on the current release and fail on the candidate",
					len(outcome.RegressedCases)),
			})
		}
	}
	return outcome, failures
}

// runFailure answers the failure of a run that did not finish.
func runFailure(datasetID int64, role string, run *RunEvidence) (GateFailure, bool) {
	failure := GateFailure{DatasetID: datasetID, Role: role}
	switch {
	case run == nil || run.Missing:
		failure.Code = FailureRunMissing
		failure.Message = "the " + role + " run no longer exists"
		if run != nil {
			failure.EvalRunID = run.EvalRunID
		}
		return failure, true
	case run.Status == evaluation.RunStatusErrored:
		failure.Code = FailureRunErrored
		failure.EvalRunID = run.EvalRunID
		failure.Message = "the " + role + " run errored"
		if run.Error != "" {
			failure.Message += ": " + truncate(run.Error, 200)
		}
		return failure, true
	case run.Status == evaluation.RunStatusCancelled:
		failure.Code = FailureRunCancelled
		failure.EvalRunID = run.EvalRunID
		failure.Message = "the " + role + " run was cancelled"
		return failure, true
	case run.Status != evaluation.RunStatusFinished:
		// Evaluate is called with terminal runs only; a run in any other
		// state is not evidence.
		failure.Code = FailureRunErrored
		failure.EvalRunID = run.EvalRunID
		failure.Message = "the " + role + " run is in state " + run.Status
		return failure, true
	}
	return GateFailure{}, false
}

// dimensionSummary is the scores of one dimension on one run.
type dimensionSummary struct {
	mean       *float64
	scored     int
	incomplete int
}

// summarise answers the mean normalised score per dimension over the `ok`
// results, and the count of results that are not `ok`.
func summarise(results []evaluation.RunResult) map[string]dimensionSummary {
	sums := map[string]float64{}
	out := map[string]dimensionSummary{}
	for _, result := range results {
		summary := out[result.DimensionID]
		if result.Status == evaluation.ResultStatusOK && result.NormalizedScore != nil {
			sums[result.DimensionID] += *result.NormalizedScore
			summary.scored++
		} else {
			summary.incomplete++
		}
		out[result.DimensionID] = summary
	}
	for id, summary := range out {
		if summary.scored > 0 {
			mean := round2(sums[id] / float64(summary.scored))
			summary.mean = &mean
			out[id] = summary
		}
	}
	return out
}

func evaluateCriterion(
	datasetID int64,
	criterion Criterion,
	candidate, baseline map[string]dimensionSummary,
	evidence SuiteEvidence,
) (CriterionOutcome, *GateFailure) {
	result := CriterionOutcome{Criterion: criterion}
	fail := func(code, message string) (CriterionOutcome, *GateFailure) {
		result.Status = CriterionFailed
		result.Note = message
		return result, &GateFailure{
			Code: code, DatasetID: datasetID, Role: RoleCandidate, EvalRunID: evidence.Candidate.EvalRunID,
			DimensionID: criterion.DimensionID, Score: result.Score, Baseline: result.Baseline,
			Required: result.Required, Message: message,
		}
	}

	scores := candidate[criterion.DimensionID]
	result.Score = scores.mean
	if scores.mean == nil {
		return fail(FailureDimensionUnscored, "the candidate has no score on dimension "+criterion.DimensionID)
	}
	if scores.incomplete > 0 {
		return fail(FailureDimensionIncomplete, fmt.Sprintf(
			"%d case(s) could not be scored on dimension %s on the candidate", scores.incomplete, criterion.DimensionID))
	}

	switch criterion.Kind {
	case CriterionThreshold:
		required := *criterion.MinScore
		result.Required = &required
		if *scores.mean < required {
			return fail(FailureThresholdNotMet, fmt.Sprintf(
				"dimension %s scored %.2f, below the threshold %.2f", criterion.DimensionID, *scores.mean, required))
		}
	case CriterionNoWorseThan:
		if baseline == nil {
			result.Status = CriterionNotApplicable
			result.Note = "there is no current release to compare with"
			return result, nil
		}
		reference := baseline[criterion.DimensionID]
		result.Baseline = reference.mean
		if reference.mean == nil {
			result.Status = CriterionFailed
			result.Note = "the current release has no score on dimension " + criterion.DimensionID
			return result, &GateFailure{
				Code: FailureDimensionUnscored, DatasetID: datasetID, Role: RoleBaseline,
				EvalRunID: evidence.Baseline.EvalRunID, DimensionID: criterion.DimensionID, Message: result.Note,
			}
		}
		required := round2(*reference.mean - *criterion.MaxDrop)
		result.Required = &required
		if *scores.mean < required {
			return fail(FailureScoreRegressed, fmt.Sprintf(
				"dimension %s scored %.2f, more than %.2f points below the current release's %.2f",
				criterion.DimensionID, *scores.mean, *criterion.MaxDrop, *reference.mean))
		}
	default:
		return fail(FailureDimensionUnscored, "unknown criterion kind "+criterion.Kind)
	}
	result.Status = CriterionPassed
	return result, nil
}

// casePass answers whether one result passed, and whether that can be
// decided. A stored target (target_met) decides first; else a threshold
// criterion's min_score on the normalised score. A result that is not `ok`
// is decided as not passed.
func casePass(result evaluation.RunResult, criterion Criterion) (bool, bool) {
	if result.Status != evaluation.ResultStatusOK || result.NormalizedScore == nil {
		return false, true
	}
	if result.TargetMet != nil {
		return *result.TargetMet, true
	}
	if criterion.Kind == CriterionThreshold && criterion.MinScore != nil {
		return *result.NormalizedScore >= *criterion.MinScore, true
	}
	return false, false
}

// regressedCases lists the cases of the regression suite that passed on
// the baseline and do not pass on the candidate, on any of the suite's
// dimensions, in case id order.
func regressedCases(suite SuiteConfig, evidence SuiteEvidence) []RegressedCase {
	type key struct{ caseID, dimensionID string }
	index := func(results []evaluation.RunResult) map[key]evaluation.RunResult {
		out := make(map[key]evaluation.RunResult, len(results))
		for _, result := range results {
			out[key{result.DatasetCaseID, result.DimensionID}] = result
		}
		return out
	}
	baseline := index(evidence.Baseline.Results)
	candidate := index(evidence.Candidate.Results)

	regressed := map[string][]string{}
	for _, criterion := range suite.Criteria {
		for cell, before := range baseline {
			if cell.dimensionID != criterion.DimensionID {
				continue
			}
			passedBefore, decided := casePass(before, criterion)
			if !decided || !passedBefore {
				continue
			}
			after, present := candidate[cell]
			if present {
				passedAfter, decidedAfter := casePass(after, criterion)
				if !decidedAfter || passedAfter {
					continue
				}
			}
			// A case the candidate did not run at all (it was excluded or
			// deleted after the baseline) is not listed: the candidate run
			// did not test it.
			if !present && !candidateRanCase(evidence.Candidate.Results, cell.caseID) {
				continue
			}
			regressed[cell.caseID] = append(regressed[cell.caseID], criterion.DimensionID)
		}
	}

	out := make([]RegressedCase, 0, len(regressed))
	for caseID, dimensions := range regressed {
		sort.Strings(dimensions)
		out = append(out, RegressedCase{CaseID: caseID, Name: evidence.CaseNames[caseID], DimensionIDs: dimensions})
	}
	sort.Slice(out, func(i, j int) bool { return lessID(out[i].CaseID, out[j].CaseID) })
	return out
}

func candidateRanCase(results []evaluation.RunResult, caseID string) bool {
	for _, result := range results {
		if result.DatasetCaseID == caseID {
			return true
		}
	}
	return false
}

// lessID orders numeric ids numerically and other strings lexically.
func lessID(a, b string) bool {
	left, leftErr := strconv.Atoi(a)
	right, rightErr := strconv.Atoi(b)
	if leftErr == nil && rightErr == nil {
		return left < right
	}
	return a < b
}

// CaseName is the name a regression listing gives a case: the start of
// its input on one line.
func CaseName(input string) string {
	return truncate(strings.Join(strings.Fields(input), " "), caseExcerptLength)
}

func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

func round2(value float64) float64 { return math.Round(value*100) / 100 }

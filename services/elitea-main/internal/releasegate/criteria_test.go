package releasegate

import (
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

func score(value float64) *float64 { return &value }

func met(value bool) *bool { return &value }

func ok(caseID, dimensionID string, normalized float64, target *bool) evaluation.RunResult {
	return evaluation.RunResult{
		DatasetCaseID: caseID, DimensionID: dimensionID, Status: evaluation.ResultStatusOK,
		NormalizedScore: score(normalized), NativeScore: score(normalized), TargetMet: target,
	}
}

func finished(id int64, results ...evaluation.RunResult) *RunEvidence {
	return &RunEvidence{EvalRunID: id, Status: evaluation.RunStatusFinished, Results: results}
}

func threshold(dimension string, min float64) Criterion {
	return Criterion{DimensionID: dimension, Kind: CriterionThreshold, MinScore: score(min)}
}

func noWorse(dimension string, drop float64) Criterion {
	return Criterion{DimensionID: dimension, Kind: CriterionNoWorseThan, MaxDrop: score(drop)}
}

func codes(failures []GateFailure) []string {
	out := make([]string, 0, len(failures))
	for _, failure := range failures {
		out = append(out, failure.Code)
	}
	return out
}

func TestEvaluateNoSuitePasses(t *testing.T) {
	state, outcome, failures := Evaluate(nil)
	if state != StatePassed || len(outcome) != 0 || len(failures) != 0 {
		t.Fatalf("Evaluate(nil) = %s %v %v, want passed with nothing", state, outcome, failures)
	}
}

func TestEvaluateThreshold(t *testing.T) {
	suite := SuiteConfig{DatasetID: 4, Criteria: []Criterion{threshold("1", 70)}}

	state, outcome, failures := Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, ok("1", "1", 80, nil), ok("2", "1", 70, nil)),
	}})
	if state != StatePassed || len(failures) != 0 {
		t.Fatalf("a mean of 75 against 70: %s %v", state, failures)
	}
	if got := outcome[0].Criteria[0]; got.Status != CriterionPassed || *got.Score != 75 || *got.Required != 70 {
		t.Fatalf("criterion outcome = %+v", got)
	}

	state, _, failures = Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, ok("1", "1", 80, nil), ok("2", "1", 50, nil)),
	}})
	if state != StateFailed || len(failures) != 1 || failures[0].Code != FailureThresholdNotMet {
		t.Fatalf("a mean of 65 against 70: %s %v", state, codes(failures))
	}
	if *failures[0].Score != 65 || failures[0].DimensionID != "1" || failures[0].EvalRunID != 10 {
		t.Fatalf("failure = %+v", failures[0])
	}
}

func TestEvaluateNoWorseThan(t *testing.T) {
	suite := SuiteConfig{DatasetID: 4, Criteria: []Criterion{noWorse("1", 5)}}
	baseline := finished(11, ok("1", "1", 80, nil), ok("2", "1", 80, nil))

	state, _, failures := Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, ok("1", "1", 76, nil), ok("2", "1", 76, nil)), Baseline: baseline,
	}})
	if state != StatePassed {
		t.Fatalf("76 against 80 - 5: %s %v", state, codes(failures))
	}

	state, _, failures = Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, ok("1", "1", 70, nil), ok("2", "1", 74, nil)), Baseline: baseline,
	}})
	if state != StateFailed || failures[0].Code != FailureScoreRegressed {
		t.Fatalf("72 against 80 - 5: %s %v", state, codes(failures))
	}
	if *failures[0].Baseline != 80 || *failures[0].Required != 75 {
		t.Fatalf("failure = %+v", failures[0])
	}

	// No current release: there is nothing to be worse than.
	state, outcome, _ := Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, ok("1", "1", 10, nil)),
	}})
	if state != StatePassed || outcome[0].Criteria[0].Status != CriterionNotApplicable {
		t.Fatalf("no release: %s %+v", state, outcome[0].Criteria[0])
	}
}

func TestEvaluateRegressionListsCasesByID(t *testing.T) {
	suite := SuiteConfig{DatasetID: 4, Regression: true, Criteria: []Criterion{threshold("1", 0), threshold("2", 0)}}
	baseline := finished(11,
		ok("7", "1", 100, met(true)), ok("7", "2", 100, met(true)),
		ok("8", "1", 100, met(true)), ok("8", "2", 0, met(false)),
		ok("9", "1", 100, met(true)), ok("9", "2", 100, met(true)),
	)
	candidate := finished(10,
		// Case 7 regresses on dimension 2.
		ok("7", "1", 100, met(true)), ok("7", "2", 0, met(false)),
		// Case 8 failed dimension 2 before as well: not a regression.
		ok("8", "1", 100, met(true)), ok("8", "2", 0, met(false)),
		// Case 9 errors on dimension 1 now: a regression.
		evaluation.RunResult{DatasetCaseID: "9", DimensionID: "1", Status: evaluation.ResultStatusError},
		ok("9", "2", 100, met(true)),
	)

	state, outcome, failures := Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: candidate, Baseline: baseline,
		CaseNames: map[string]string{"7": "What is Go?", "9": "And Rust?"},
	}})
	if state != StateFailed {
		t.Fatalf("state = %s", state)
	}
	var regression *GateFailure
	for index := range failures {
		if failures[index].Code == FailureRegression {
			regression = &failures[index]
		}
	}
	if regression == nil {
		t.Fatalf("no regression failure in %v", codes(failures))
	}
	want := []RegressedCase{
		{CaseID: "7", Name: "What is Go?", DimensionIDs: []string{"2"}},
		{CaseID: "9", Name: "And Rust?", DimensionIDs: []string{"1"}},
	}
	if len(regression.Cases) != len(want) {
		t.Fatalf("regressed cases = %+v", regression.Cases)
	}
	for index, item := range want {
		got := regression.Cases[index]
		if got.CaseID != item.CaseID || got.Name != item.Name || len(got.DimensionIDs) != 1 || got.DimensionIDs[0] != item.DimensionIDs[0] {
			t.Errorf("case %d = %+v, want %+v", index, got, item)
		}
	}
	if len(outcome[0].RegressedCases) != 2 {
		t.Errorf("the outcome does not list the regressed cases: %+v", outcome[0])
	}
}

func TestEvaluateRunThatDidNotFinishFailsTheGate(t *testing.T) {
	suite := SuiteConfig{DatasetID: 4, Criteria: []Criterion{threshold("1", 0)}}
	cases := map[string]*RunEvidence{
		FailureRunErrored:   {EvalRunID: 10, Status: evaluation.RunStatusErrored, Error: "the dataset could not be read"},
		FailureRunCancelled: {EvalRunID: 10, Status: evaluation.RunStatusCancelled},
		FailureRunMissing:   {EvalRunID: 10, Missing: true},
	}
	for code, run := range cases {
		state, outcome, failures := Evaluate([]SuiteEvidence{{Suite: suite, Candidate: run}})
		if state != StateFailed || len(failures) != 1 || failures[0].Code != code {
			t.Errorf("%s: %s %v", code, state, codes(failures))
		}
		if outcome[0].Criteria[0].Status != CriterionNotEvaluated {
			t.Errorf("%s: criterion = %+v", code, outcome[0].Criteria[0])
		}
	}

	// An errored BASELINE fails the gate too.
	state, _, failures := Evaluate([]SuiteEvidence{{
		Suite:     SuiteConfig{DatasetID: 4, Regression: true, Criteria: []Criterion{threshold("1", 0)}},
		Candidate: finished(10, ok("1", "1", 90, nil)),
		Baseline:  &RunEvidence{EvalRunID: 11, Status: evaluation.RunStatusErrored},
	}})
	if state != StateFailed || failures[0].Code != FailureRunErrored || failures[0].Role != RoleBaseline {
		t.Fatalf("errored baseline: %s %+v", state, failures)
	}
}

func TestEvaluateUnscoredAndIncompleteDimensions(t *testing.T) {
	suite := SuiteConfig{DatasetID: 4, Criteria: []Criterion{threshold("1", 50)}}

	_, _, failures := Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10, evaluation.RunResult{DatasetCaseID: "1", DimensionID: "1", Status: evaluation.ResultStatusError}),
	}})
	if len(failures) != 1 || failures[0].Code != FailureDimensionUnscored {
		t.Fatalf("no ok score: %v", codes(failures))
	}

	// One case scored 100 and one errored: the mean of the one is not
	// accepted for the dimension.
	_, _, failures = Evaluate([]SuiteEvidence{{
		Suite: suite, Candidate: finished(10,
			ok("1", "1", 100, nil),
			evaluation.RunResult{DatasetCaseID: "2", DimensionID: "1", Status: evaluation.ResultStatusError}),
	}})
	if len(failures) != 1 || failures[0].Code != FailureDimensionIncomplete {
		t.Fatalf("one case errored: %v", codes(failures))
	}
}

func TestEvaluateNotRunnableAndSkippedSuites(t *testing.T) {
	state, outcome, failures := Evaluate([]SuiteEvidence{
		{Suite: SuiteConfig{DatasetID: 4, Criteria: []Criterion{threshold("1", 0)}}, NotRunnable: "dataset 4 was deleted"},
		{Suite: SuiteConfig{DatasetID: 5, Criteria: []Criterion{threshold("1", 0)}}, Skipped: true},
	})
	if state != StateFailed || len(failures) != 1 || failures[0].Code != FailureSuiteNotRunnable || failures[0].DatasetID != 4 {
		t.Fatalf("%s %+v", state, failures)
	}
	if outcome[1].Criteria[0].Status != CriterionNotEvaluated {
		t.Fatalf("skipped suite outcome = %+v", outcome[1])
	}
}

func TestSuiteConfigValidate(t *testing.T) {
	days := 0
	bad := []SuiteConfig{
		{ApplicationID: 1, DatasetID: 2},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{{DimensionID: "x", Kind: CriterionThreshold, MinScore: score(1)}}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{{DimensionID: "1", Kind: CriterionThreshold}}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{{DimensionID: "1", Kind: CriterionThreshold, MinScore: score(101)}}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{{DimensionID: "1", Kind: CriterionNoWorseThan, MinScore: score(1)}}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{{DimensionID: "1", Kind: "average", MinScore: score(1)}}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{threshold("1", 1), noWorse("1", 2)}},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{threshold("1", 1)}, ReevaluateEveryDays: &days},
		{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{
			threshold("1", 1), threshold("2", 1), threshold("3", 1), threshold("4", 1), threshold("5", 1), threshold("6", 1),
		}},
	}
	for index, suite := range bad {
		if err := suite.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("config %d: err = %v, want ErrInvalidConfig", index, err)
		}
	}
	good := SuiteConfig{ApplicationID: 1, DatasetID: 2, Criteria: []Criterion{threshold("1", 70), noWorse("2", 5)}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if !good.NeedsBaseline() {
		t.Error("a no_worse_than criterion needs a baseline")
	}
}

func TestCaseName(t *testing.T) {
	long := ""
	for range 200 {
		long += "a"
	}
	if got := CaseName("  what\n is   Go? "); got != "what is Go?" {
		t.Errorf("CaseName = %q", got)
	}
	if got := []rune(CaseName(long)); len(got) != caseExcerptLength {
		t.Errorf("CaseName length = %d", len(got))
	}
}

func TestLoosens(t *testing.T) {
	current := &SuiteConfig{Regression: true, Criteria: []Criterion{
		{DimensionID: "1", Kind: CriterionThreshold, MinScore: score(70)},
		{DimensionID: "2", Kind: CriterionNoWorseThan, MaxDrop: score(5)},
	}}
	with := func(edit func(*SuiteConfig)) *SuiteConfig {
		next := *current
		next.Criteria = append([]Criterion(nil), current.Criteria...)
		edit(&next)
		return &next
	}
	for _, c := range []struct {
		name          string
		current, next *SuiteConfig
		want          bool
	}{
		{"new suite", nil, current, false},
		{"unchanged", current, with(func(*SuiteConfig) {}), false},
		{"unmark", current, nil, true},
		{"unset regression", current, with(func(s *SuiteConfig) { s.Regression = false }), true},
		{"remove a criterion", current, with(func(s *SuiteConfig) { s.Criteria = s.Criteria[:1] }), true},
		{"lower min_score", current, with(func(s *SuiteConfig) { s.Criteria[0].MinScore = score(60) }), true},
		{"raise min_score", current, with(func(s *SuiteConfig) { s.Criteria[0].MinScore = score(80) }), false},
		{"raise max_drop", current, with(func(s *SuiteConfig) { s.Criteria[1].MaxDrop = score(10) }), true},
		{"lower max_drop", current, with(func(s *SuiteConfig) { s.Criteria[1].MaxDrop = score(2) }), false},
		{"change a kind", current, with(func(s *SuiteConfig) {
			s.Criteria[0] = Criterion{DimensionID: "1", Kind: CriterionNoWorseThan, MaxDrop: score(0)}
		}), true},
		{"add a criterion", current, with(func(s *SuiteConfig) {
			s.Criteria = append(s.Criteria, Criterion{DimensionID: "3", Kind: CriterionThreshold, MinScore: score(50)})
		}), false},
		{"drop the schedule", current, with(func(s *SuiteConfig) { s.ReevaluateEveryDays = nil }), false},
	} {
		if got := Loosens(c.current, c.next); got != c.want {
			t.Errorf("%s: Loosens = %v, want %v", c.name, got, c.want)
		}
	}
}

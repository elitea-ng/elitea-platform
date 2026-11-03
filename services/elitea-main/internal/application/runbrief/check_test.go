package runbrief

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func threeConditions() Brief {
	return Brief{
		Goal: "Ship the export",
		DoneWhen: []Condition{
			{ID: "c1", Text: "The button exists"}, {ID: "c2", Text: "The test passes"}, {ID: "c3", Text: "Docs updated"},
		},
		Constraints: []string{}, Assumptions: []string{}, Ambiguity: AmbiguityLow,
	}
}

func verdicts(values ...string) []ConditionResult {
	out := []ConditionResult{}
	for index, value := range values {
		id := []string{"c1", "c2", "c3"}[index]
		out = append(out, ConditionResult{ID: id, Verdict: value, Reason: "reason " + id})
	}
	return out
}

func TestParseVerdictsKeepsTheBriefOrderAndText(t *testing.T) {
	got, err := ParseVerdicts(`{"conditions":[
		{"id":"c3","verdict":"NOT_MET","reason":"no docs\nyet"},
		{"id":"c1","verdict":"met","reason":"seen"},
		{"id":"c2","verdict":"impossible","reason":"no runner"}]}`, threeConditions())
	require.NoError(t, err)
	require.Equal(t, []ConditionResult{
		{ID: "c1", Text: "The button exists", Verdict: VerdictMet, Reason: "seen"},
		{ID: "c2", Text: "The test passes", Verdict: VerdictImpossible, Reason: "no runner"},
		{ID: "c3", Text: "Docs updated", Verdict: VerdictNotMet, Reason: "no docs yet"},
	}, got)
}

func TestParseVerdictsRefusesAnIncompleteAnswer(t *testing.T) {
	for name, answer := range map[string]string{
		"missing":   `{"conditions":[{"id":"c1","verdict":"met"},{"id":"c2","verdict":"met"}]}`,
		"unknown":   `{"conditions":[{"id":"c1","verdict":"met"},{"id":"c2","verdict":"met"},{"id":"c9","verdict":"met"}]}`,
		"twice":     `{"conditions":[{"id":"c1","verdict":"met"},{"id":"c1","verdict":"met"},{"id":"c2","verdict":"met"}]}`,
		"verdict":   `{"conditions":[{"id":"c1","verdict":"maybe"},{"id":"c2","verdict":"met"},{"id":"c3","verdict":"met"}]}`,
		"not json":  `All done!`,
		"extra one": `{"conditions":[{"id":"c1","verdict":"met"},{"id":"c2","verdict":"met"},{"id":"c3","verdict":"met"},{"id":"c4","verdict":"met"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseVerdicts(answer, threeConditions())
			require.ErrorIs(t, err, ErrInvalidVerdicts)
		})
	}
}

func TestDecideAllMet(t *testing.T) {
	result, counts := Decide(verdicts(VerdictMet, VerdictMet, VerdictMet), map[string]int{"c2": 2})
	require.Equal(t, OutcomeMet, result.Outcome)
	require.Equal(t, 3, result.Met)
	require.Equal(t, 3, result.Total)
	require.Empty(t, result.Reason)
	require.Empty(t, counts, "a met condition clears its count")
}

func TestDecideNotMetCountsAndNamesTheFirstFailingReason(t *testing.T) {
	result, counts := Decide(verdicts(VerdictMet, VerdictNotMet, VerdictNotMet), map[string]int{"c1": 1, "c2": 1})
	require.Equal(t, OutcomeNotMet, result.Outcome)
	require.Equal(t, "reason c2", result.Reason)
	require.Equal(t, 1, result.Met)
	require.Equal(t, map[string]int{"c2": 2, "c3": 1}, counts)
}

func TestDecideBlocksOnTheThirdConsecutiveNotMetOfOneCondition(t *testing.T) {
	failures := map[string]int{}
	var result Result
	for check := 1; check <= BlockedAfter; check++ {
		result, failures = Decide(verdicts(VerdictMet, VerdictNotMet, VerdictMet), failures)
		if check < BlockedAfter {
			require.Equal(t, OutcomeNotMet, result.Outcome, "check %d", check)
		}
	}
	require.Equal(t, OutcomeBlocked, result.Outcome)
	require.Equal(t, "reason c2", result.Reason)
	require.Equal(t, map[string]int{"c2": 3}, failures)
}

func TestDecideCountsOnlyConsecutiveChecks(t *testing.T) {
	failures := map[string]int{}
	_, failures = Decide(verdicts(VerdictNotMet, VerdictMet, VerdictMet), failures)
	_, failures = Decide(verdicts(VerdictNotMet, VerdictMet, VerdictMet), failures)
	_, failures = Decide(verdicts(VerdictMet, VerdictMet, VerdictMet), failures)
	result, failures := Decide(verdicts(VerdictNotMet, VerdictMet, VerdictMet), failures)
	require.Equal(t, OutcomeNotMet, result.Outcome)
	require.Equal(t, map[string]int{"c1": 1}, failures)
	// Different conditions failing in turn never block.
	_, failures = Decide(verdicts(VerdictMet, VerdictNotMet, VerdictMet), failures)
	result, _ = Decide(verdicts(VerdictMet, VerdictMet, VerdictNotMet), failures)
	require.Equal(t, OutcomeNotMet, result.Outcome)
}

func TestDecideImpossibleBlocksAtOnce(t *testing.T) {
	result, _ := Decide(verdicts(VerdictNotMet, VerdictImpossible, VerdictMet), nil)
	require.Equal(t, OutcomeBlocked, result.Outcome)
	require.Equal(t, "reason c2", result.Reason)
}

func TestPublicDropsTheContinueText(t *testing.T) {
	result := Result{Outcome: OutcomeNotMet, ContinueText: "Continue"}
	public := result.Public()
	require.Empty(t, public.ContinueText)
	require.Equal(t, []ConditionResult{}, public.Conditions)
}

func TestParseVerdictsIgnoresTextAfterTheObject(t *testing.T) {
	object := `{"conditions":[{"id":"c1","verdict":"met"},{"id":"c2","verdict":"met"},{"id":"c3","verdict":"met"}]}`
	for name, answer := range map[string]string{
		"trailing prose":           object + "\nAll three are met {see above}.",
		"fenced with prose inside": "```json\n" + object + "\nnote: checked the diff\n```",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseVerdicts(answer, threeConditions())
			require.NoError(t, err)
			require.Len(t, got, 3)
		})
	}
}

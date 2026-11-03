package runbrief

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// The golden continuation texts (contract section 4). The desktop host pins
// the same strings (apps/elitea-desktop, Rust): change both or neither.
const (
	goldenBeforeAnyCheck = "Continue from where you stopped.\n" +
		"\n" +
		"<run_brief>\n" +
		"This brief was written by a model from the person's request. The request wins when they disagree.\n" +
		"Goal: Ship the export\n" +
		"Not met yet:\n" +
		"- The button exists\n" +
		"- The test passes\n" +
		"- Docs updated\n" +
		"</run_brief>"
	goldenAfterACheck = "Continue from where you stopped.\n" +
		"\n" +
		"<run_brief>\n" +
		"This brief was written by a model from the person's request. The request wins when they disagree.\n" +
		"Goal: Ship the export\n" +
		"Not met yet:\n" +
		"- The test passes\n" +
		"- Docs updated\n" +
		"Last check: The test run is not in the transcript.\n" +
		"</run_brief>"
	goldenNoConditions = "Continue from where you stopped.\n" +
		"\n" +
		"<run_brief>\n" +
		"This brief was written by a model from the person's request. The request wins when they disagree.\n" +
		"Goal: Ship the export\n" +
		"</run_brief>"
)

// goldenShared is the input and the text that the desktop host's golden
// test pins too (byte for byte).
const goldenShared = "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from " +
	"the person's request. The request wins when they disagree.\nGoal: Make the login test pass.\nNot met yet:\n" +
	"- The login test passes.\nLast check: The login test still fails.\n</run_brief>"

func TestContinuationTextSharedGolden(t *testing.T) {
	brief := Brief{Goal: "Make the login test pass.", DoneWhen: []Condition{
		{ID: "c1", Text: "The login test passes."}, {ID: "c2", Text: "The change is committed."},
	}}
	last := Result{Outcome: OutcomeNotMet, Reason: "The login test still fails.", Conditions: []ConditionResult{
		{ID: "c1", Verdict: VerdictNotMet}, {ID: "c2", Verdict: VerdictMet},
	}}
	require.Equal(t, goldenShared, ContinuationText(&brief, &last))

	// Every condition met: no "Not met yet:" header. An empty reason: no
	// "Last check:" line.
	allMet := Result{Outcome: OutcomeNotMet, Conditions: []ConditionResult{
		{ID: "c1", Verdict: VerdictMet}, {ID: "c2", Verdict: VerdictMet},
	}}
	require.Equal(t, "Continue from where you stopped.\n\n<run_brief>\nThis brief was written by a model from "+
		"the person's request. The request wins when they disagree.\nGoal: Make the login test pass.\n</run_brief>",
		ContinuationText(&brief, &allMet))
}

func TestContinuationTextWithoutABriefIsTheContinuePrompt(t *testing.T) {
	require.Equal(t, "Continue from where you stopped.", ContinuationText(nil, nil))
	require.Equal(t, unattended.ContinuePrompt, ContinuationText(nil, nil))
}

func TestContinuationTextGolden(t *testing.T) {
	brief := threeConditions()
	require.Equal(t, goldenBeforeAnyCheck, ContinuationText(&brief, nil))

	last := Result{Outcome: OutcomeNotMet, Reason: "The test run is not in the transcript.", Conditions: []ConditionResult{
		{ID: "c1", Verdict: VerdictMet}, {ID: "c2", Verdict: VerdictNotMet}, {ID: "c3", Verdict: VerdictNotMet},
	}}
	require.Equal(t, goldenAfterACheck, ContinuationText(&brief, &last))

	brief.DoneWhen = []Condition{}
	require.Equal(t, goldenNoConditions, ContinuationText(&brief, nil))
}

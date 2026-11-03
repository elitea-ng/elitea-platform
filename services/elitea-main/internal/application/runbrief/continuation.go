package runbrief

import (
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// The continuation text (contract section 4). The desktop host
// (apps/elitea-desktop) builds the same text; both sides pin it with a
// golden test, so change it on both sides or on neither.
const (
	// BriefOpen starts the brief block, after ContinuePrompt and one empty
	// line.
	BriefOpen = "<run_brief>"
	// BriefNotice is the first line of the block.
	BriefNotice = "This brief was written by a model from the person's request. The request wins when they disagree."
	// BriefGoalLabel, BriefOpenLabel and BriefLastCheckLabel start their
	// lines.
	BriefGoalLabel      = "Goal: "
	BriefOpenLabel      = "Not met yet:"
	BriefItemPrefix     = "- "
	BriefLastCheckLabel = "Last check: "
	// BriefClose ends the block. The text has no final line break.
	BriefClose = "</run_brief>"
)

// ContinuationText is the user text of a continuation of a chain: only
// unattended.ContinuePrompt without a ready brief, else ContinuePrompt and
// the brief block. last is the chain's latest goal check, nil before the
// first one.
//
//	Continue from where you stopped.
//
//	<run_brief>
//	This brief was written by a model from the person's request. The request wins when they disagree.
//	Goal: {goal}
//	Not met yet:
//	- {text of each open condition}
//	Last check: {reason}
//	</run_brief>
//
// "Not met yet" lists the conditions that the last check did not find met
// (every condition before the first check); the block is left out when
// done_when is empty or every condition is met. "Last check" is left out
// before the first check and when the last reason is empty. The
// brief's text was cleaned when it was stored and is inserted as it is.
func ContinuationText(brief *Brief, last *Result) string {
	if brief == nil {
		return unattended.ContinuePrompt
	}
	lines := []string{unattended.ContinuePrompt, "", BriefOpen, BriefNotice, BriefGoalLabel + brief.Goal}
	if open := openConditions(brief, last); len(open) > 0 {
		lines = append(lines, BriefOpenLabel)
		for _, text := range open {
			lines = append(lines, BriefItemPrefix+text)
		}
	}
	if last != nil && last.Outcome != OutcomeNotChecked && last.Reason != "" {
		lines = append(lines, BriefLastCheckLabel+last.Reason)
	}
	lines = append(lines, BriefClose)
	return strings.Join(lines, "\n")
}

// openConditions are the texts of the conditions that the last check did
// not find met, in the brief's order.
func openConditions(brief *Brief, last *Result) []string {
	met := map[string]bool{}
	if last != nil {
		for _, condition := range last.Conditions {
			if condition.Verdict == VerdictMet {
				met[condition.ID] = true
			}
		}
	}
	open := []string{}
	for _, condition := range brief.DoneWhen {
		if !met[condition.ID] {
			open = append(open, condition.Text)
		}
	}
	return open
}

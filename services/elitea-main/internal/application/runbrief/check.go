package runbrief

import (
	"errors"
	"fmt"
	"strings"
)

// Outcomes of a goal check.
const (
	OutcomeMet        = "met"
	OutcomeNotMet     = "not_met"
	OutcomeBlocked    = "blocked"
	OutcomeNotChecked = "not_checked"
)

// Verdicts of one condition.
const (
	VerdictMet        = "met"
	VerdictNotMet     = "not_met"
	VerdictImpossible = "impossible"
)

// BlockedAfter is how many consecutive checks may find one condition not
// met before the chain ends as failed.
const BlockedAfter = 3

// ConditionResult is one condition's verdict.
type ConditionResult struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// Result is one goal check (the contract's section 3 object).
type Result struct {
	Outcome    string            `json:"outcome"`
	Conditions []ConditionResult `json:"conditions"`
	Met        int               `json:"met"`
	Total      int               `json:"total"`
	Reason     string            `json:"reason,omitempty"`
	// ContinueText is the user text of the goal_not_met continuation; only
	// for not_met.
	ContinueText string `json:"continue_text,omitempty"`
}

// Public is the result as the transcript shows it: without continue_text.
func (r Result) Public() Result {
	r.ContinueText = ""
	if r.Conditions == nil {
		r.Conditions = []ConditionResult{}
	}
	return r
}

// NotChecked is the result of a check that could not run.
func NotChecked(brief *Brief) Result {
	total := 0
	if brief != nil {
		total = len(brief.DoneWhen)
	}
	return Result{Outcome: OutcomeNotChecked, Conditions: []ConditionResult{}, Total: total}
}

// ErrInvalidVerdicts is a model answer that does not judge each condition
// of the brief once.
var ErrInvalidVerdicts = errors.New("runbrief: the answer does not judge each condition")

type rawVerdicts struct {
	Conditions []struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	} `json:"conditions"`
}

// ParseVerdicts reads the checker's answer: one verdict for each condition
// of the brief, in the brief's order, with the brief's own text.
func ParseVerdicts(answer string, brief Brief) ([]ConditionResult, error) {
	var raw rawVerdicts
	if err := decodeObject(answer, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidVerdicts, err)
	}
	byID := map[string]ConditionResult{}
	for _, entry := range raw.Conditions {
		id := strings.TrimSpace(entry.ID)
		verdict := strings.ToLower(strings.TrimSpace(entry.Verdict))
		switch verdict {
		case VerdictMet, VerdictNotMet, VerdictImpossible:
		default:
			return nil, fmt.Errorf("%w: verdict %q", ErrInvalidVerdicts, entry.Verdict)
		}
		if _, seen := byID[id]; seen {
			return nil, fmt.Errorf("%w: condition %q twice", ErrInvalidVerdicts, id)
		}
		byID[id] = ConditionResult{ID: id, Verdict: verdict, Reason: Clean(entry.Reason, MaxReasonChars)}
	}
	if len(byID) != len(brief.DoneWhen) {
		return nil, fmt.Errorf("%w: %d verdicts for %d conditions", ErrInvalidVerdicts, len(byID), len(brief.DoneWhen))
	}
	out := make([]ConditionResult, 0, len(brief.DoneWhen))
	for _, condition := range brief.DoneWhen {
		verdict, ok := byID[condition.ID]
		if !ok {
			return nil, fmt.Errorf("%w: no verdict for %q", ErrInvalidVerdicts, condition.ID)
		}
		verdict.Text = condition.Text
		out = append(out, verdict)
	}
	return out, nil
}

// Decide applies the outcome rules to one check's verdicts and the chain's
// consecutive not_met counts. It answers the result (without continue_text)
// and the new counts.
//
//   - Any impossible, or a condition not_met on BlockedAfter consecutive
//     checks: blocked. The reason is that condition's reason.
//   - Otherwise any not_met: not_met. The reason is the first not_met
//     condition's reason.
//   - Otherwise: met.
//
// A condition that is met clears its count.
func Decide(verdicts []ConditionResult, failures map[string]int) (Result, map[string]int) {
	result := Result{Conditions: verdicts, Total: len(verdicts)}
	counts := map[string]int{}
	var impossible, exhausted, notMet *ConditionResult
	for index := range verdicts {
		verdict := &verdicts[index]
		switch verdict.Verdict {
		case VerdictMet:
			result.Met++
		case VerdictNotMet:
			counts[verdict.ID] = failures[verdict.ID] + 1
			if notMet == nil {
				notMet = verdict
			}
			if exhausted == nil && counts[verdict.ID] >= BlockedAfter {
				exhausted = verdict
			}
		case VerdictImpossible:
			if impossible == nil {
				impossible = verdict
			}
		}
	}
	switch {
	case impossible != nil:
		result.Outcome, result.Reason = OutcomeBlocked, impossible.Reason
	case exhausted != nil:
		result.Outcome, result.Reason = OutcomeBlocked, exhausted.Reason
	case notMet != nil:
		result.Outcome, result.Reason = OutcomeNotMet, notMet.Reason
	default:
		result.Outcome = OutcomeMet
	}
	return result, counts
}

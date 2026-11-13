package evaluation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Enhance with AI: the proposal a finished run produces.
//
// ONE model call reads the run's PINNED agent definition and the results that
// missed their targets, and answers three kinds of fix. None is applied here.
// The proposal is data; the UI's review wizard saves each accepted fix through
// the route that already owns that write (agent version, dimension update,
// dataset case update), so every permission and the ADR-0040 change-request
// path stay where they are.
//
// The model call is the judge's Completer, the same gateway hop the three
// AI-draft routes use. It carries no attribution id, so the proposal's spend
// never lands in the run's own analytics (`eval:<run>:` is a run's agent and
// judge spend and nothing else).

const (
	// MaxEnhanceMissedRows bounds the evidence sent to the model. A run holds
	// at most MaxCasesPerDataset x MaxDimensionsPerRun results, so the bound
	// is a prompt-size guard, and the truncation is reported on the wire.
	MaxEnhanceMissedRows = 20
	// MaxEnhanceFieldRunes bounds each case text (input, output, expected,
	// rationale) inside the prompt.
	MaxEnhanceFieldRunes = 1500
	// MaxEnhanceInstructionRunes bounds the instruction text sent and accepted.
	MaxEnhanceInstructionRunes = 24000
	maxEnhanceRationale        = 1000
	maxEnhanceSummary          = 1500
)

// EnhanceInstructionPatch is the proposed replacement for the pinned
// version's instructions. It carries the whole new text, not a diff, so the
// wizard shows its own diff and the new version is saved verbatim.
type EnhanceInstructionPatch struct {
	Rationale    string `json:"rationale"`
	Instructions string `json:"instructions"`
}

// EnhanceDimensionChange is a rubric and/or target change for one dimension
// of the run's snapshot. A field the model left alone is omitted.
type EnhanceDimensionChange struct {
	DimensionID           string   `json:"dimension_id"`
	DimensionName         string   `json:"dimension_name"`
	Description           *string  `json:"description,omitempty"`
	DefaultTarget         *float64 `json:"default_target,omitempty"`
	DefaultTargetOperator string   `json:"default_target_operator,omitempty"`
	Rationale             string   `json:"rationale"`
}

// EnhanceDatasetChange is a new expected output for one missed case.
type EnhanceDatasetChange struct {
	CaseID    string `json:"case_id"`
	DatasetID string `json:"dataset_id"`
	// Input and CurrentExpectedOutput show the reviewer what the change is
	// against. CurrentExpectedOutput is null when the case has none.
	Input                 string  `json:"input"`
	CurrentExpectedOutput *string `json:"current_expected_output"`
	ExpectedOutput        string  `json:"expected_output"`
	Rationale             string  `json:"rationale"`
}

// EnhanceProposal is the response.
type EnhanceProposal struct {
	RunID                string                   `json:"run_id"`
	ApplicationID        *int                     `json:"application_id"`
	ApplicationVersionID *int                     `json:"application_version_id"`
	Summary              string                   `json:"summary"`
	AnalysedRows         int                      `json:"analysed_rows"`
	MissedRows           int                      `json:"missed_rows"`
	InstructionPatch     *EnhanceInstructionPatch `json:"instruction_patch"`
	DimensionChanges     []EnhanceDimensionChange `json:"dimension_changes"`
	DatasetChanges       []EnhanceDatasetChange   `json:"dataset_changes"`
}

// missedRow is one (case, dimension) result that missed its target.
type missedRow struct {
	Result    RunResult
	Case      DatasetCase
	Dimension SnapshotDimension
	Binding   SnapshotBinding
}

// missedRows picks the results to analyse: scored `ok` with `target_met`
// false. Results with no decidable target are not misses (nothing says what
// good was), and `error` rows are not scores. Order: lowest normalised score
// first, so the cap keeps the worst.
func missedRows(run Run, results []RunResult, cases []DatasetCase) []missedRow {
	byCase := make(map[string]DatasetCase, len(cases))
	for _, testCase := range cases {
		byCase[testCase.ID] = testCase
	}
	bindings := make(map[string]SnapshotBinding, len(run.Snapshot.Bindings))
	for _, binding := range run.Snapshot.Bindings {
		bindings[binding.DimensionID] = binding
	}
	rows := make([]missedRow, 0)
	for _, result := range results {
		if result.Status != ResultStatusOK || result.TargetMet == nil || *result.TargetMet {
			continue
		}
		testCase, known := byCase[result.DatasetCaseID]
		dimension, dimKnown := run.Snapshot.Dimensions[result.DimensionID]
		if !known || !dimKnown {
			continue
		}
		rows = append(rows, missedRow{Result: result, Case: testCase, Dimension: dimension, Binding: bindings[result.DimensionID]})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return normalizedOrZero(rows[i].Result) < normalizedOrZero(rows[j].Result)
	})
	return rows
}

func normalizedOrZero(result RunResult) float64 {
	if result.NormalizedScore == nil {
		return 0
	}
	return *result.NormalizedScore
}

const enhanceSystemPrompt = `You improve an AI agent after an evaluation run. You are given the agent's current instructions, the scoring dimensions (each with its rubric and target) and the cases that MISSED their targets, with the agent's answer, the expected answer if any, the score and the judge's rationale.

Propose fixes of three kinds. Propose only what the evidence supports; an empty list or null is a correct answer.
1. instruction_patch: a full revised version of the agent's instructions that fixes the observed failures. Keep what works; change what the evidence blames.
2. dimension_changes: a rubric (description) or target change for a dimension whose rubric is ambiguous, or whose target is unreachable or too lax given the evidence. Use only the dimension ids you were given.
3. dataset_changes: a corrected expected_output for a case whose expected answer is wrong, outdated or too narrow, judging by the agent's answer and the judge's rationale. Use only the case ids you were given.

Reply with a single JSON object and nothing else, in exactly this shape:
{"summary": "<two or three sentences on what went wrong>",
 "instruction_patch": null | {"rationale": "<why>", "instructions": "<the complete new instructions>"},
 "dimension_changes": [{"dimension_id": "<id>", "description": "<new rubric, omit to keep>", "default_target": <number, omit to keep>, "default_target_operator": "<one of >=, >, <=, <, ==, omit to keep>", "rationale": "<why>"}],
 "dataset_changes": [{"case_id": "<id>", "expected_output": "<new expected answer>", "rationale": "<why>"}]}

Do not wrap the JSON in a code fence. Do not add any text before or after it.`

// buildEnhancePrompt renders the user turn. Everything in it is DATA about the
// run; the system prompt is the only instruction.
func buildEnhancePrompt(instructions string, run Run, rows []missedRow) string {
	var out strings.Builder
	out.WriteString("The agent's current instructions:\n<<<\n")
	out.WriteString(truncateRunes(instructions, MaxEnhanceInstructionRunes))
	out.WriteString("\n>>>\n\nScoring dimensions:\n")
	ids := make([]string, 0, len(run.Snapshot.Dimensions))
	for id := range run.Snapshot.Dimensions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dimension := run.Snapshot.Dimensions[id]
		fmt.Fprintf(&out, "- id=%s name=%q scale=%s %s..%s polarity=%s", id, dimension.Name, dimension.ScaleType,
			formatScaleBound(dimension.ScaleMin), formatScaleBound(dimension.ScaleMax), dimension.Polarity)
		for _, binding := range run.Snapshot.Bindings {
			if binding.DimensionID == id && binding.Target != nil {
				fmt.Fprintf(&out, " target=%s %g", binding.TargetOperator, *binding.Target)
			}
		}
		fmt.Fprintf(&out, "\n  rubric: %s\n", truncateRunes(fallback(dimension.Description, "(none authored)"), MaxEnhanceFieldRunes))
	}
	out.WriteString("\nCases that missed their targets:\n")
	for index, row := range rows {
		fmt.Fprintf(&out, "\n[%d] case_id=%s dimension_id=%s score=%s\n", index+1, row.Case.ID, row.Result.DimensionID, scoreText(row.Result))
		fmt.Fprintf(&out, "input: %s\n", truncateRunes(row.Case.Input, MaxEnhanceFieldRunes))
		fmt.Fprintf(&out, "agent answer: %s\n", truncateRunes(evidenceText(row.Result, "output"), MaxEnhanceFieldRunes))
		if row.Case.ExpectedOutput != nil && *row.Case.ExpectedOutput != "" {
			fmt.Fprintf(&out, "expected answer: %s\n", truncateRunes(*row.Case.ExpectedOutput, MaxEnhanceFieldRunes))
		}
		reason, _ := row.Result.Verdict["reason"].(string)
		fmt.Fprintf(&out, "judge rationale: %s\n", truncateRunes(fallback(reason, "(none)"), MaxEnhanceFieldRunes))
	}
	return out.String()
}

func scoreText(result RunResult) string {
	if result.NativeScore == nil {
		return "n/a"
	}
	return fmt.Sprintf("%g", *result.NativeScore)
}

func evidenceText(result RunResult, key string) string {
	text, _ := result.Evidence[key].(string)
	return text
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + " [...truncated]"
}

// rawProposal is the model's answer, before validation.
type rawProposal struct {
	Summary          string `json:"summary"`
	InstructionPatch *struct {
		Rationale    string `json:"rationale"`
		Instructions string `json:"instructions"`
	} `json:"instruction_patch"`
	DimensionChanges []struct {
		DimensionID           string   `json:"dimension_id"`
		Description           *string  `json:"description"`
		DefaultTarget         *float64 `json:"default_target"`
		DefaultTargetOperator string   `json:"default_target_operator"`
		Rationale             string   `json:"rationale"`
	} `json:"dimension_changes"`
	DatasetChanges []struct {
		CaseID         string  `json:"case_id"`
		ExpectedOutput *string `json:"expected_output"`
		Rationale      string  `json:"rationale"`
	} `json:"dataset_changes"`
}

// parseEnhanceAnswer finds the proposal object in a model answer. Like the
// judge's parse it scans from the end for the last balanced object that
// decodes AND carries a non-empty summary, so reasoning prose that quotes the
// schema (or a nested object that decodes to nothing) is skipped.
func parseEnhanceAnswer(raw string) (rawProposal, bool) {
	for start := strings.LastIndex(raw, "{"); start >= 0; start = strings.LastIndex(raw[:start], "{") {
		end, closed := matchingBrace(raw, start)
		if !closed {
			continue
		}
		var parsed rawProposal
		if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
			continue
		}
		if strings.TrimSpace(parsed.Summary) != "" {
			return parsed, true
		}
	}
	return rawProposal{}, false
}

// validateProposal keeps only what the run can honour. A fix that names a
// dimension or case outside the evidence, an operator outside the set, an
// empty replacement or a no-op is dropped, never repaired: a proposal that
// reached the wizard is one the normal endpoints will accept.
func validateProposal(raw rawProposal, instructions string, run Run, rows []missedRow) EnhanceProposal {
	proposal := EnhanceProposal{
		RunID:                run.ID,
		ApplicationID:        run.ApplicationID,
		ApplicationVersionID: run.ApplicationVersionID,
		Summary:              truncateRunes(strings.TrimSpace(raw.Summary), maxEnhanceSummary),
		DimensionChanges:     []EnhanceDimensionChange{},
		DatasetChanges:       []EnhanceDatasetChange{},
	}
	if patch := raw.InstructionPatch; patch != nil {
		next := strings.TrimSpace(patch.Instructions)
		if next != "" && next != strings.TrimSpace(instructions) && len([]rune(next)) <= MaxEnhanceInstructionRunes {
			proposal.InstructionPatch = &EnhanceInstructionPatch{
				Rationale:    truncateRunes(strings.TrimSpace(patch.Rationale), maxEnhanceRationale),
				Instructions: next,
			}
		}
	}

	missedDimension := map[string]bool{}
	cases := map[string]DatasetCase{}
	for _, row := range rows {
		missedDimension[row.Result.DimensionID] = true
		cases[row.Case.ID] = row.Case
	}
	seenDimension := map[string]bool{}
	for _, change := range raw.DimensionChanges {
		dimension, known := run.Snapshot.Dimensions[change.DimensionID]
		if !known || !missedDimension[change.DimensionID] || seenDimension[change.DimensionID] {
			continue
		}
		out := EnhanceDimensionChange{
			DimensionID:   change.DimensionID,
			DimensionName: dimension.Name,
			Rationale:     truncateRunes(strings.TrimSpace(change.Rationale), maxEnhanceRationale),
		}
		if change.Description != nil {
			text := strings.TrimSpace(*change.Description)
			if text != "" && text != dimension.Description && len(text) <= MaxDimensionDescriptionBytes {
				out.Description = &text
			}
		}
		operator := strings.TrimSpace(change.DefaultTargetOperator)
		if change.DefaultTarget != nil && knownOperators[operator] {
			out.DefaultTarget = change.DefaultTarget
			out.DefaultTargetOperator = operator
		}
		if out.Description == nil && out.DefaultTarget == nil {
			continue
		}
		seenDimension[change.DimensionID] = true
		proposal.DimensionChanges = append(proposal.DimensionChanges, out)
	}
	seenCase := map[string]bool{}
	for _, change := range raw.DatasetChanges {
		testCase, known := cases[change.CaseID]
		if !known || seenCase[change.CaseID] || change.ExpectedOutput == nil {
			continue
		}
		text := strings.TrimSpace(*change.ExpectedOutput)
		if text == "" || len(text) > MaxCaseInputBytes || (testCase.ExpectedOutput != nil && text == strings.TrimSpace(*testCase.ExpectedOutput)) {
			continue
		}
		seenCase[change.CaseID] = true
		proposal.DatasetChanges = append(proposal.DatasetChanges, EnhanceDatasetChange{
			CaseID:                change.CaseID,
			DatasetID:             testCase.DatasetID,
			Input:                 truncateRunes(testCase.Input, MaxEnhanceFieldRunes),
			ExpectedOutput:        text,
			Rationale:             truncateRunes(strings.TrimSpace(change.Rationale), maxEnhanceRationale),
			CurrentExpectedOutput: testCase.ExpectedOutput,
		})
	}
	return proposal
}

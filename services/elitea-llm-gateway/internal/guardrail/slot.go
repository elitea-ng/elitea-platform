package guardrail

// The content scope of a detector and the prompt_injection slot (ADR-0043
// decision 4).
//
// # content: all | tool_results
//
// A detector of an input row reads every text part of the request by
// default (`content: all`). With `content: tool_results` it reads only the
// tool results of the request: a Chat `tool` message, a Responses
// function_call_output, custom_tool_call_output or shell call output, and an
// Anthropic tool_result. The gateway marks those parts
// (internal/llmproxy/guardrail_slot.go). An output row refuses
// `tool_results`: an answer has no tool results.
//
// # slot: prompt_injection
//
// A detector with `slot: prompt_injection` fills the prompt-injection slot.
// The slot needs an input row and `content: tool_results`, so a firm attaches
// a classifier to tool output, the untrusted data of decision 4. Every
// finding of the detector gets the label `prompt_injection`, whatever label
// the classifier gave. The action must be block or flag: a mask replaces a
// span, and an injection is not a span to remove. The secrets and presidio
// detectors cannot fill the slot: they find data, not instructions.
//
// Every verdict of a slot detector names the slot: Verdict.Slot is set and
// the labels hold `slot:prompt_injection`, also on a pass. The labels are the
// part of the verdict that gateway.guardrail_verdicts stores.
//
// # Two runs on one request
//
// CheckInput runs the input stage twice: once with the `all` detectors on
// every part, then once with the `tool_results` detectors on the tool-result
// parts only. Both runs read the original parts. Their mask spans are merged
// and applied once, as in one run. Each run applies the row timeout on its
// own. A request without a tool result does not run the `tool_results`
// detectors and writes no verdict for them.

import (
	"context"
	"fmt"
	"strings"
)

// Content scopes of a detector.
const (
	ContentAll         = "all"
	ContentToolResults = "tool_results"
)

// SlotPromptInjection is the prompt-injection slot and the label of its
// findings.
const SlotPromptInjection = "prompt_injection"

// SlotLabel is the verdict label that names a slot.
func SlotLabel(slot string) string { return "slot:" + slot }

// parseDetectorScope reads the content and slot options of a detector.
func parseDetectorScope(d *Detector, obj map[string]any) error {
	d.Content = ContentAll
	if v, present := obj["content"]; present && v != nil {
		s, _ := v.(string)
		switch s {
		case ContentAll, ContentToolResults:
			d.Content = s
		default:
			return fmt.Errorf("content must be %q or %q", ContentAll, ContentToolResults)
		}
	}
	v, present := obj["slot"]
	if !present || v == nil || v == "" {
		return nil
	}
	if s, _ := v.(string); s != SlotPromptInjection {
		return fmt.Errorf("slot must be %q", SlotPromptInjection)
	}
	if d.Content != ContentToolResults {
		return fmt.Errorf("slot %q needs content %q: the slot reads tool results only", SlotPromptInjection, ContentToolResults)
	}
	if d.Action == ActionMask {
		return fmt.Errorf("slot %q allows the actions block and flag; mask cannot remove an injection", SlotPromptInjection)
	}
	if d.Type == TypeSecrets || d.Type == TypePresidio {
		return fmt.Errorf("a %s detector cannot fill slot %q: it finds data, not instructions", d.Type, SlotPromptInjection)
	}
	d.Slot = SlotPromptInjection
	return nil
}

// checkRowScopes refuses content tool_results on an output row.
func checkRowScopes(row *Row) error {
	if row.Stage == StageInput {
		return nil
	}
	for i, d := range row.Detectors {
		if d.Content == ContentToolResults {
			return fmt.Errorf("guardrail.detectors[%d]: content %q applies to input rows only: an answer has no tool results", i, ContentToolResults)
		}
	}
	return nil
}

// slotFindings gives every finding of a slot detector the slot label.
func slotFindings(d *Detector, findings []Finding) []Finding {
	if d.Slot == "" {
		return findings
	}
	for i := range findings {
		findings[i].Label = d.Slot
	}
	return findings
}

// withSlotLabel returns the labels of v with the slot label added.
func withSlotLabel(v Verdict) []string {
	if v.Slot == "" {
		return v.Labels
	}
	return append(append([]string(nil), v.Labels...), SlotLabel(v.Slot))
}

func contentAllDetectors(d *Detector) bool { return d.Content != ContentToolResults }

func toolResultDetectors(d *Detector) bool { return d.Content == ContentToolResults }

// ScopesToolResults reports whether a detector of the pipeline reads tool
// results only. The gateway then marks the tool-result parts and calls
// CheckInput.
func (p *Pipeline) ScopesToolResults() bool {
	if !p.Active() {
		return false
	}
	for _, row := range p.Rows {
		for _, d := range row.Detectors {
			if d.Content == ContentToolResults {
				return true
			}
		}
	}
	return false
}

// CheckInput is CheckAll for the input stage with content scopes. toolResult
// marks the parts that are tool results. The `all` detectors read every
// part; the `tool_results` detectors read the marked parts only. It returns
// each part masked, or the refusing verdict. A part with no mark is not a
// tool result.
func (p *Pipeline) CheckInput(ctx context.Context, parts []string, toolResult []bool) ([]string, *Verdict) {
	if !p.Active() || len(parts) == 0 {
		return parts, nil
	}
	spans := make([][]span, len(parts))

	all := make([]int, len(parts))
	for i := range parts {
		all[i] = i
	}
	if blocked := p.runSubset(ctx, parts, all, contentAllDetectors, spans); blocked != nil {
		return nil, blocked
	}
	var tools []int
	for i := range parts {
		if i < len(toolResult) && toolResult[i] {
			tools = append(tools, i)
		}
	}
	if len(tools) > 0 {
		if blocked := p.runSubset(ctx, parts, tools, toolResultDetectors, spans); blocked != nil {
			return nil, blocked
		}
	}
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = applyMasks(part, spans[i])
	}
	return out, nil
}

// runSubset runs the filtered detectors on the parts named by idx, joined as
// CheckAll joins them, and adds the mask spans to spans by part.
func (p *Pipeline) runSubset(ctx context.Context, parts []string, idx []int, filter detectorFilter, spans [][]span) *Verdict {
	var b strings.Builder
	bounds := make([]bound, len(idx))
	for n, i := range idx {
		if n > 0 {
			b.WriteString(segmentSeparator)
		}
		start := b.Len()
		b.WriteString(parts[i])
		bounds[n] = bound{start: start, end: b.Len()}
	}
	var res result
	if len(idx) == 1 {
		res = p.run(ctx, b.String(), filter)
	} else {
		res = p.runParts(ctx, b.String(), bounds, filter)
	}
	if res.blocked != nil {
		return res.blocked
	}
	for _, s := range res.spans {
		for n, bd := range bounds {
			if s.end <= bd.start || s.start >= bd.end {
				continue
			}
			plen := bd.end - bd.start
			spans[idx[n]] = append(spans[idx[n]], span{
				start: clamp(s.start-bd.start, 0, plen), end: clamp(s.end-bd.start, 0, plen), label: s.label,
			})
		}
	}
	return nil
}

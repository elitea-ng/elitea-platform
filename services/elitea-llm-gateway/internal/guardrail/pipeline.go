package guardrail

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Pipeline is the guardrail rows that apply to one request at one stage, with
// the run-time environment and the request's verdict collector.
type Pipeline struct {
	Stage     string
	Rows      []*Row
	Env       Env
	Collector *Collector
}

// Active reports whether the pipeline has a row to run.
func (p *Pipeline) Active() bool { return p != nil && len(p.Rows) > 0 }

// Holds reports whether a block or mask detector applies, so streamed output
// must be held back until it is checked. A flag-only pipeline holds nothing.
func (p *Pipeline) Holds() bool {
	if !p.Active() {
		return false
	}
	for _, row := range p.Rows {
		for _, d := range row.Detectors {
			if liveDetector(d) {
				return true
			}
		}
	}
	return false
}

// span is one byte range to mask, with the label of the first detector that
// asked for it.
type span struct {
	start, end int
	label      string
}

// segmentLocal marks a detector whose matches must not cross the boundary of
// a part, so it reads each part alone (units.go). The secrets detector is
// one: its private-key rule matches through the end of the text when a PEM
// block has no END line, and that end is the end of the part, not the end of
// the whole request.
type segmentLocal interface {
	segmentLocal()
}

// bound is a byte range.
type bound struct{ start, end int }

// result is the outcome of one pipeline run on one content.
type result struct {
	blocked *Verdict
	spans   []span
}

// detectorFilter selects the detectors one run evaluates.
type detectorFilter func(*Detector) bool

func allDetectors(*Detector) bool { return true }

// Check runs every detector of the pipeline on content. It returns the
// content with every mask span replaced, or blocked with the verdict of the
// detector that blocked (or failed closed).
func (p *Pipeline) Check(ctx context.Context, content string) (masked string, blocked *Verdict) {
	if !p.Active() {
		return content, nil
	}
	res := p.run(ctx, content, allDetectors)
	if res.blocked != nil {
		return "", res.blocked
	}
	return applyMasks(content, res.spans), nil
}

// run evaluates the selected detectors on one content.
func (p *Pipeline) run(ctx context.Context, content string, filter detectorFilter) result {
	blocked, spans := p.runRows(ctx, []string{content}, nil, filter)
	if blocked != nil {
		return result{blocked: blocked}
	}
	return result{spans: spans[0]}
}

// runRows evaluates the selected detectors of every row on parts, in row
// order and in detector order. tool marks the parts that are tool results.
// Each row runs under ONE deadline, its timeout. A block, or a failure of a
// closed row, stops the run: a later row sends no content anywhere. It
// returns the refusing verdict, or the mask spans of each part.
func (p *Pipeline) runRows(ctx context.Context, parts []string, tool []bool, filter detectorFilter) (*Verdict, [][]span) {
	spans := make([][]span, len(parts))
	for _, row := range p.Rows {
		rowCtx, cancel := context.WithTimeout(ctx, row.Timeout)
		blocked := p.runRow(rowCtx, row, parts, tool, filter, spans)
		cancel()
		if blocked != nil {
			return blocked, nil
		}
	}
	return nil, spans
}

// detectorParts returns the indexes of the parts that d reads: every part
// for content all, the tool results for content tool_results.
func detectorParts(d *Detector, n int, tool []bool) []int {
	idx := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if d.Content != ContentToolResults || isToolPart(tool, i) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (p *Pipeline) runRow(ctx context.Context, row *Row, parts []string, tool []bool, filter detectorFilter, spans [][]span) *Verdict {
	for _, d := range row.Detectors {
		if !filter(d) {
			continue
		}
		idx := detectorParts(d, len(parts), tool)
		if len(idx) == 0 {
			// No part is in the scope of the detector (a tool_results
			// detector on a request without a tool result): no run, no
			// verdict.
			continue
		}
		started := time.Now()
		views, partial := detectorViews(d, parts, tool, idx)
		perView, err := p.detectViews(ctx, d, views)
		latency := time.Since(started)
		v := Verdict{
			Stage: p.Stage, Row: row.Name, Detector: d.Name, DetectorType: d.Type,
			Action: d.Action, FailMode: row.FailMode, Slot: d.Slot,
		}
		if err != nil {
			v.Labels = []string{LabelDetectorError}
			if row.FailMode == FailClosed {
				v.Outcome = OutcomeErrorClosed
				p.Collector.add(v, latency)
				return &v
			}
			v.Outcome = OutcomeErrorOpen
			p.Collector.add(v, latency)
			continue
		}
		var found []partSpan
		for i := range views {
			for _, f := range slotFindings(d, perView[i]) {
				v.Findings++
				v.Labels = append(v.Labels, f.Label)
				if f.Score > v.MaxScore {
					v.MaxScore = f.Score
				}
				found = append(found, views[i].spans(f)...)
			}
		}
		if partial {
			v.Labels = append(v.Labels, LabelPartialCoverage)
		}
		switch {
		case v.Findings == 0:
			v.Outcome = OutcomePass
		case d.Action == ActionBlock:
			v.Outcome = OutcomeBlocked
			p.Collector.add(v, latency)
			return &v
		case d.Action == ActionMask:
			v.Outcome = OutcomeMasked
			for _, s := range found {
				spans[s.part] = append(spans[s.part], s.span)
			}
		case d.Action == ActionWarn:
			v.Outcome = OutcomeWarned
		default:
			v.Outcome = OutcomeFlagged
		}
		p.Collector.add(v, latency)
	}
	return nil
}

// MaskMarker returns the text that replaces a masked span.
func MaskMarker(label string) string { return "[REDACTED:" + label + "]" }

// mergeSpans sorts spans and joins overlapping or touching ones. The merged
// span keeps the label of its first member.
func mergeSpans(spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	sorted := append([]span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].end > sorted[j].end
	})
	out := []span{sorted[0]}
	for _, s := range sorted[1:] {
		last := &out[len(out)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// applyMasks replaces every span of content with its marker.
func applyMasks(content string, spans []span) string {
	return applyMasksFrom(content, spans, false)
}

// applyMasksFrom is applyMasks. With continued set, a span that starts at
// byte 0 continues a mask that an earlier release already marked: it removes
// its text and writes no second marker.
func applyMasksFrom(content string, spans []span, continued bool) string {
	merged := mergeSpans(spans)
	if len(merged) == 0 {
		return content
	}
	var b strings.Builder
	b.Grow(len(content))
	pos := 0
	for _, s := range merged {
		start, end := clamp(s.start, 0, len(content)), clamp(s.end, 0, len(content))
		if start < pos {
			start = pos
		}
		if end <= start {
			continue
		}
		b.WriteString(content[pos:start])
		if !continued || start != 0 {
			b.WriteString(MaskMarker(s.label))
		}
		pos = end
	}
	b.WriteString(content[pos:])
	return b.String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// segmentSeparator joins the parts that one detector call reads (units.go).
const segmentSeparator = "\n\n"

// CheckAll runs the pipeline on parts and returns each part masked, or
// blocked. A detector reads the parts as units.go describes. A mask span
// that covers a separator masks both parts it touches.
func (p *Pipeline) CheckAll(ctx context.Context, parts []string) ([]string, *Verdict) {
	return p.checkParts(ctx, parts, nil)
}

// checkParts runs every detector on parts; tool marks the tool results.
func (p *Pipeline) checkParts(ctx context.Context, parts []string, tool []bool) ([]string, *Verdict) {
	if !p.Active() || len(parts) == 0 {
		return parts, nil
	}
	blocked, spans := p.runRows(ctx, parts, tool, allDetectors)
	if blocked != nil {
		return nil, blocked
	}
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = applyMasks(part, spans[i])
	}
	return out, nil
}

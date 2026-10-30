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

// span is one byte range to mask, with the label of the first detector that
// asked for it.
type span struct {
	start, end int
	label      string
}

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

// run evaluates the selected detectors of every row, in row order and in
// detector order. A block, or a failure of a closed row, stops the run.
func (p *Pipeline) run(ctx context.Context, content string, filter detectorFilter) result {
	var res result
	for _, row := range p.Rows {
		rowCtx, cancel := context.WithTimeout(ctx, row.Timeout)
		stop := p.runRow(rowCtx, row, content, filter, &res)
		cancel()
		if stop {
			return res
		}
	}
	return res
}

func (p *Pipeline) runRow(ctx context.Context, row *Row, content string, filter detectorFilter, res *result) bool {
	for _, d := range row.Detectors {
		if !filter(d) {
			continue
		}
		started := time.Now()
		findings, err := d.impl.detect(ctx, p.Env, p.Stage, content)
		latency := time.Since(started)
		v := Verdict{
			Stage: p.Stage, Row: row.Name, Detector: d.Name, DetectorType: d.Type,
			Action: d.Action, FailMode: row.FailMode,
		}
		if err != nil {
			v.Labels = []string{LabelDetectorError}
			if row.FailMode == FailClosed {
				v.Outcome = OutcomeErrorClosed
				p.Collector.add(v, latency)
				res.blocked = &v
				return true
			}
			v.Outcome = OutcomeErrorOpen
			p.Collector.add(v, latency)
			continue
		}
		v.Findings = len(findings)
		for _, f := range findings {
			v.Labels = append(v.Labels, f.Label)
			if f.Score > v.MaxScore {
				v.MaxScore = f.Score
			}
		}
		if len(findings) == 0 {
			v.Outcome = OutcomePass
			p.Collector.add(v, latency)
			continue
		}
		switch d.Action {
		case ActionBlock:
			v.Outcome = OutcomeBlocked
			p.Collector.add(v, latency)
			res.blocked = &v
			return true
		case ActionMask:
			v.Outcome = OutcomeMasked
			for _, f := range findings {
				if f.Whole {
					res.spans = append(res.spans, span{start: 0, end: len(content), label: f.Label})
				} else {
					res.spans = append(res.spans, span{start: f.Start, end: f.End, label: f.Label})
				}
			}
		default:
			v.Outcome = OutcomeFlagged
		}
		p.Collector.add(v, latency)
	}
	return false
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
		b.WriteString(MaskMarker(s.label))
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

// segmentSeparator joins the parts CheckAll inspects. A detector reads the
// parts as one content, so an http adapter costs one round trip per stage.
const segmentSeparator = "\n\n"

// CheckAll runs the pipeline once on parts joined by a blank line, and returns
// each part masked, or blocked. A mask span that covers a separator masks
// both parts it touches.
func (p *Pipeline) CheckAll(ctx context.Context, parts []string) ([]string, *Verdict) {
	if !p.Active() || len(parts) == 0 {
		return parts, nil
	}
	if len(parts) == 1 {
		out, blocked := p.Check(ctx, parts[0])
		return []string{out}, blocked
	}
	joined := strings.Join(parts, segmentSeparator)
	res := p.run(ctx, joined, allDetectors)
	if res.blocked != nil {
		return nil, res.blocked
	}
	out := make([]string, len(parts))
	offset := 0
	for i, part := range parts {
		end := offset + len(part)
		var local []span
		for _, s := range res.spans {
			if s.end <= offset || s.start >= end {
				continue
			}
			local = append(local, span{start: clamp(s.start-offset, 0, len(part)), end: clamp(s.end-offset, 0, len(part)), label: s.label})
		}
		out[i] = applyMasks(part, local)
		offset = end + len(segmentSeparator)
	}
	return out, nil
}

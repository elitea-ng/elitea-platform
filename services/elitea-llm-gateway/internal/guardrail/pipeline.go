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
// a part. CheckAll joins the parts into one content; such a detector reads
// each part alone. The secrets detector is one: its private-key rule matches
// through the end of the text when a PEM block has no END line, and that end
// is the end of the part, not the end of the whole request.
type segmentLocal interface {
	segmentLocal()
}

// bound is the byte range of one part inside the joined content.
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

// run evaluates the selected detectors of every row, in row order and in
// detector order. A block, or a failure of a closed row, stops the run.
func (p *Pipeline) run(ctx context.Context, content string, filter detectorFilter) result {
	return p.runParts(ctx, content, nil, filter)
}

// runParts is run on a content joined from parts. A segmentLocal detector
// reads each part alone; the others read the whole content. Nil parts make
// the whole content one part.
func (p *Pipeline) runParts(ctx context.Context, content string, parts []bound, filter detectorFilter) result {
	var res result
	for _, row := range p.Rows {
		rowCtx, cancel := context.WithTimeout(ctx, row.Timeout)
		stop := p.runRow(rowCtx, row, content, parts, filter, &res)
		cancel()
		if stop {
			return res
		}
	}
	return res
}

// detectParts runs d on each part of content and moves the findings to
// offsets in content. A finding without a span covers its part only.
func (p *Pipeline) detectParts(ctx context.Context, d *Detector, content string, parts []bound) ([]Finding, error) {
	var out []Finding
	for _, b := range parts {
		found, err := d.impl.detect(ctx, p.Env, p.Stage, content[b.start:b.end])
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			if f.Whole {
				f.Whole = false
				f.Start, f.End = 0, b.end-b.start
			}
			f.Start += b.start
			f.End += b.start
			out = append(out, f)
		}
	}
	return out, nil
}

func (p *Pipeline) runRow(ctx context.Context, row *Row, content string, parts []bound, filter detectorFilter, res *result) bool {
	for _, d := range row.Detectors {
		if !filter(d) {
			continue
		}
		started := time.Now()
		var (
			findings []Finding
			err      error
		)
		if _, local := d.impl.(segmentLocal); local && len(parts) > 1 {
			findings, err = p.detectParts(ctx, d, content, parts)
		} else {
			findings, err = d.impl.detect(ctx, p.Env, p.Stage, content)
		}
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

// segmentSeparator joins the parts CheckAll inspects. A detector reads the
// parts as one content, so an http adapter costs one round trip per stage.
const segmentSeparator = "\n\n"

// CheckAll runs the pipeline once on parts joined by a blank line, and returns
// each part masked, or blocked. A mask span that covers a separator masks
// both parts it touches. A segmentLocal detector (secrets) reads each part
// alone, so its spans never cross a separator.
func (p *Pipeline) CheckAll(ctx context.Context, parts []string) ([]string, *Verdict) {
	if !p.Active() || len(parts) == 0 {
		return parts, nil
	}
	if len(parts) == 1 {
		out, blocked := p.Check(ctx, parts[0])
		return []string{out}, blocked
	}
	joined := strings.Join(parts, segmentSeparator)
	bounds := make([]bound, len(parts))
	offset := 0
	for i, part := range parts {
		bounds[i] = bound{start: offset, end: offset + len(part)}
		offset += len(part) + len(segmentSeparator)
	}
	res := p.runParts(ctx, joined, bounds, allDetectors)
	if res.blocked != nil {
		return nil, res.blocked
	}
	out := make([]string, len(parts))
	offset = 0
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

package guardrail

// units.go — the unit of analysis: what one detector call reads.
//
// A stage inspects a list of parts: the text of each message, each tool
// result and each tool-call argument value. A detector call reads a "view",
// and the pipeline maps every finding of a view back to the parts it covers.
// There are three kinds of detector:
//
//   - A detector without a limit (regex, the generic http adapter) reads all
//     its parts in one view, joined by segmentSeparator. So an http adapter
//     costs one round trip per stage.
//   - A segmentLocal detector (secrets) reads each part in its own view: a
//     match never crosses a message boundary.
//   - A boundedReader (the vendor adapters) has a vendor limit on the text of
//     one call. Its views are "units". A unit holds whole consecutive parts,
//     joined by segmentSeparator, up to the limit. A unit never mixes tool
//     results with other parts. A part that is longer than the limit is split
//     into units of at most the limit, at a white space where one is near.
//
// # The call bound
//
// A detector makes at most maxDetectorCalls unit calls on one run. When the
// parts need more units, the detector reads the NEWEST units only (the end
// of the conversation) and the verdict gets the label LabelPartialCoverage.
// The skipped text was the newest text of an earlier request in a normal
// conversation, so an earlier run read it. A long conversation is thus never
// a detector error.
//
// # Concurrency
//
// The unit calls of one detector run in parallel, at most
// detectorConcurrency at a time. They share the row deadline. The first error
// cancels the other calls of that detector.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// maxDetectorCalls bounds the unit calls of one detector on one run.
const maxDetectorCalls = 16

// detectorConcurrency bounds the unit calls of one detector in flight.
const detectorConcurrency = 4

// splitLookback is how far back (in bytes) a split looks for a white space.
const splitLookback = 256

// LabelPartialCoverage is the verdict label of a detector that read only the
// newest maxDetectorCalls units of the content.
const LabelPartialCoverage = "partial_coverage"

// boundedReader is a detector with a vendor limit on the text of one call.
// textLimit is in UTF-16 code units (textCost), the strictest count that a
// vendor uses for "characters".
type boundedReader interface {
	textLimit() int
}

// toolResultReader is a detector that reads a unit of tool results in a
// different way from other content (Azure Prompt Shields reads it as a
// document). The pipeline calls detectToolResults for such a unit.
type toolResultReader interface {
	detectToolResults(ctx context.Context, env Env, stage, content string) ([]Finding, error)
}

// seg is one piece of one part inside a view. partStart is the byte offset
// of the piece in its part; viewStart and viewEnd are its bytes in the view.
type seg struct {
	part, partStart, viewStart, viewEnd int
}

// view is the text of one detector call.
type view struct {
	text string
	segs []seg
	// tool is true when every part of the view is a tool result.
	tool bool
}

// partSpan is a byte range in one part.
type partSpan struct {
	part int
	span
}

// spans maps a finding of the view to byte ranges in the parts it covers. A
// Whole finding covers every piece of the view.
func (v *view) spans(f Finding) []partSpan {
	s, e := f.Start, f.End
	if f.Whole {
		s, e = 0, len(v.text)
	}
	var out []partSpan
	for _, sg := range v.segs {
		if e <= sg.viewStart || s >= sg.viewEnd {
			continue
		}
		n := sg.viewEnd - sg.viewStart
		start := sg.partStart + clamp(s-sg.viewStart, 0, n)
		end := sg.partStart + clamp(e-sg.viewStart, 0, n)
		if end > start {
			out = append(out, partSpan{part: sg.part, span: span{start: start, end: end, label: f.Label}})
		}
	}
	return out
}

func isToolPart(tool []bool, i int) bool { return i < len(tool) && tool[i] }

// joinedView returns one view of the parts idx, joined by segmentSeparator.
func joinedView(parts []string, tool []bool, idx []int) view {
	var b strings.Builder
	v := view{tool: true}
	for n, i := range idx {
		if n > 0 {
			b.WriteString(segmentSeparator)
		}
		start := b.Len()
		b.WriteString(parts[i])
		v.segs = append(v.segs, seg{part: i, viewStart: start, viewEnd: b.Len()})
		v.tool = v.tool && isToolPart(tool, i)
	}
	v.text = b.String()
	return v
}

// partViews returns one view per part.
func partViews(parts []string, tool []bool, idx []int) []view {
	out := make([]view, 0, len(idx))
	for _, i := range idx {
		out = append(out, view{text: parts[i], segs: []seg{{part: i, viewEnd: len(parts[i])}}, tool: isToolPart(tool, i)})
	}
	return out
}

// runeCost is the UTF-16 length of r.
func runeCost(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// textCost is the UTF-16 length of s.
func textCost(s string) int {
	n := 0
	for _, r := range s {
		n += runeCost(r)
	}
	return n
}

// splitText splits s into ranges of at most limit UTF-16 code units. A range
// never splits a character. When a white space is in the last splitLookback
// bytes of a full range, the range ends after it, so a word stays whole.
func splitText(s string, limit int) []bound {
	var out []bound
	start, cost, space := 0, 0, -1
	for i, r := range s {
		c := runeCost(r)
		if cost+c > limit && i > start {
			cut := i
			if space > start && i-space <= splitLookback {
				cut = space
			}
			out = append(out, bound{start: start, end: cut})
			start, cost, space = cut, textCost(s[cut:i]), -1
		}
		cost += c
		if unicode.IsSpace(r) {
			space = i + utf8.RuneLen(r)
		}
	}
	return append(out, bound{start: start, end: len(s)})
}

// unitViews packs the parts idx into units of at most limit UTF-16 code
// units. It returns the newest maxDetectorCalls units, and partial when it
// dropped older ones. An empty part needs no unit.
func unitViews(parts []string, tool []bool, idx []int, limit int) (views []view, partial bool) {
	sepCost := textCost(segmentSeparator)
	var (
		b       strings.Builder
		cur     *view
		curCost int
	)
	flush := func() {
		if cur != nil {
			cur.text = b.String()
			views = append(views, *cur)
			cur, curCost = nil, 0
			b.Reset()
		}
	}
	for _, i := range idx {
		text := parts[i]
		if text == "" {
			continue
		}
		t := isToolPart(tool, i)
		cost := textCost(text)
		if cost > limit {
			flush()
			for _, r := range splitText(text, limit) {
				views = append(views, view{text: text[r.start:r.end], segs: []seg{{part: i, partStart: r.start, viewEnd: r.end - r.start}}, tool: t})
			}
			continue
		}
		if cur != nil && (cur.tool != t || curCost+sepCost+cost > limit) {
			flush()
		}
		if cur == nil {
			cur = &view{tool: t}
		} else {
			b.WriteString(segmentSeparator)
			curCost += sepCost
		}
		start := b.Len()
		b.WriteString(text)
		cur.segs = append(cur.segs, seg{part: i, viewStart: start, viewEnd: b.Len()})
		curCost += cost
	}
	flush()
	if len(views) > maxDetectorCalls {
		return views[len(views)-maxDetectorCalls:], true
	}
	return views, false
}

// detectorViews returns the views that d reads for the parts idx.
func detectorViews(d *Detector, parts []string, tool []bool, idx []int) (views []view, partial bool) {
	if br, ok := d.impl.(boundedReader); ok {
		return unitViews(parts, tool, idx, br.textLimit())
	}
	if _, ok := d.impl.(segmentLocal); ok {
		return partViews(parts, tool, idx), false
	}
	return []view{joinedView(parts, tool, idx)}, false
}

// detectViews runs d on every view, at most detectorConcurrency calls at a
// time, and returns the findings of each view in view order. The first error
// cancels the calls that remain.
func (p *Pipeline) detectViews(ctx context.Context, d *Detector, views []view) ([][]Finding, error) {
	call := func(ctx context.Context, v *view) ([]Finding, error) {
		if tr, ok := d.impl.(toolResultReader); ok && v.tool {
			return tr.detectToolResults(ctx, p.Env, p.Stage, v.text)
		}
		return d.impl.detect(ctx, p.Env, p.Stage, v.text)
	}
	out := make([][]Finding, len(views))
	if len(views) == 1 {
		f, err := call(ctx, &views[0])
		out[0] = f
		return out, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	fail := func(err error) {
		once.Do(func() { firstErr = err; cancel() })
	}
	sem := make(chan struct{}, detectorConcurrency)
launch:
	for i := range views {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			fail(fmt.Errorf("%w: timeout", errAdapter))
			break launch
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			f, err := call(ctx, &views[i])
			if err != nil {
				fail(err)
				return
			}
			out[i] = f
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

package guardrail

// The streaming output stage.
//
// # Latency
//
// A block or mask detector must see a match before the client does, so the
// filter holds the newest text back. It evaluates when it holds 2 × window
// bytes, and then releases all but the last window bytes. The client so
// reads the answer at most 2 × window bytes behind the model (the default
// window is 256 bytes, about 50 to 120 tokens). Each evaluation runs every
// block and mask detector once, an http adapter included: one adapter round
// trip per window of output.
//
// A flag detector changes nothing the client reads, so it adds no delay: it
// runs once, on the whole answer, when the stream ends.
//
// # Why the raw text of a mask match never reaches the client
//
// The detectors read the held text plus the last window bytes already
// released (the look-behind context). A match no longer than the window is
// complete inside the held text before any of it is released: it starts
// before the release cut only when at least a window of text follows its
// start. A match that crosses the release cut moves the cut back to the start
// of the match, so the match is released only as a whole, masked. The test
// TestStreamNeverReleasesRawMaskMatch splits secrets at every byte.
//
// # A match longer than the hold
//
// A PEM private key is one match from its BEGIN line through its END line
// (or through the end of the text when END has not arrived yet). Such a match
// can be longer than the window and spans many chunks. The filter holds it
// until it is resolved: until the END line arrives and a window of text
// follows it. The hold is bounded: when the filter holds forceReleaseFactor
// windows, it releases the held part of the match masked, and the match stays
// OPEN. While a match is open, the look-behind context starts at the start of
// the match (not at the last window), so the next evaluation matches the
// whole block again and masks the new text up to the END line. The
// continuation writes no second marker. An open look-behind longer than
// maxOpenContextBytes cannot be kept: the filter then masks all the rest of
// the channel. It releases no byte of the block raw in any case
// (TestStreamMasksAWholePEMBlock).

import (
	"context"
	"strings"
	"unicode/utf8"
)

// maxFlagBytes bounds the answer text kept for the flag detectors.
const maxFlagBytes = 1 << 20

// forceReleaseFactor bounds the held text: when a match keeps crossing the
// cut (a match longer than the window), the filter releases it masked once it
// holds this many windows.
const forceReleaseFactor = 16

// maxOpenContextBytes bounds the look-behind context of an open match (see
// "A match longer than the hold"). Past it, the filter masks the rest of the
// channel.
const maxOpenContextBytes = 64 << 10

// StreamFilter is the output stage for one streamed text channel (one choice
// of a chat completion, one content part of a Responses answer).
type StreamFilter struct {
	p        *Pipeline
	window   int
	live     bool
	flagging bool

	tail    string
	pending string
	// open is set while a mask match that reached the end of the evaluated
	// text was released masked. tail then starts at the start of the match,
	// and the next release writes no second marker for it.
	open bool
	// maskRest is the label that masks all the rest of the channel, after an
	// open match outgrew maxOpenContextBytes.
	maskRest string
	// nextEval is the held length at which the next evaluation runs after an
	// evaluation that released nothing. A held match so costs one detector
	// run (one adapter call) per window of new text, not one per delta.
	nextEval int

	full          strings.Builder
	fullTruncated bool
	released      strings.Builder
	done          bool
}

func liveDetector(d *Detector) bool { return d.Action == ActionBlock || d.Action == ActionMask }
func flagDetector(d *Detector) bool { return d.Action == ActionFlag }

// NewStream returns a filter for one text channel of the answer.
func (p *Pipeline) NewStream() *StreamFilter {
	f := &StreamFilter{p: p, window: DefaultStreamWindow}
	if !p.Active() {
		return f
	}
	f.window = 0
	for _, row := range p.Rows {
		if row.StreamWindow > f.window {
			f.window = row.StreamWindow
		}
		for _, d := range row.Detectors {
			if liveDetector(d) {
				f.live = true
			} else {
				f.flagging = true
			}
		}
	}
	return f
}

// Buffers reports whether the filter holds text back (a block or mask
// detector applies).
func (f *StreamFilter) Buffers() bool { return f.live }

// Released returns all the text the filter has released so far, masked.
func (f *StreamFilter) Released() string { return f.released.String() }

// Push adds a delta of the answer. It returns the text the client may read
// now (possibly empty), or blocked with the verdict that ended the stream.
func (f *StreamFilter) Push(ctx context.Context, delta string) (string, *Verdict) {
	if f.done || delta == "" {
		return "", nil
	}
	if f.flagging && !f.fullTruncated {
		if f.full.Len()+len(delta) > maxFlagBytes {
			f.fullTruncated = true
		} else {
			f.full.WriteString(delta)
		}
	}
	if !f.live {
		f.released.WriteString(delta)
		return delta, nil
	}
	f.pending += delta
	if len(f.pending) < 2*f.window || len(f.pending) < f.nextEval {
		return "", nil
	}
	return f.evaluate(ctx, false)
}

// Finish ends the channel: it releases the held text (masked) and runs the
// flag detectors on the whole answer. Call it once, at the end of the
// channel's text.
func (f *StreamFilter) Finish(ctx context.Context) (string, *Verdict) {
	if f.done {
		return "", nil
	}
	out := ""
	if f.live && f.pending != "" {
		var blocked *Verdict
		out, blocked = f.evaluate(ctx, true)
		if blocked != nil {
			return "", blocked
		}
	}
	f.done = true
	if f.flagging && f.full.Len() > 0 {
		res := f.p.run(ctx, f.full.String(), flagDetector)
		if res.blocked != nil {
			// A closed flag row whose detector failed. The text is already
			// out; the stream still ends with the refusal.
			return "", res.blocked
		}
	}
	return out, nil
}

func (f *StreamFilter) evaluate(ctx context.Context, final bool) (string, *Verdict) {
	text := f.tail + f.pending
	base := len(f.tail)
	res := f.p.run(ctx, text, liveDetector)
	if res.blocked != nil {
		f.done = true
		f.pending = ""
		return "", res.blocked
	}
	if f.maskRest != "" {
		res.spans = append(res.spans, span{start: base, end: len(text), label: f.maskRest})
	}
	spans := make([]span, 0, len(res.spans))
	// openStart is the start, in text, of a match that runs to the end of
	// the text: it can still grow with the next delta.
	openStart := -1
	for _, s := range res.spans {
		if s.end >= len(text) && s.end > base && (openStart < 0 || s.start < openStart) {
			openStart = s.start
		}
		s.start -= base
		s.end -= base
		if s.end <= 0 {
			continue // inside text already released and checked
		}
		if s.start < 0 {
			s.start = 0
		}
		spans = append(spans, s)
	}

	cut := len(f.pending)
	if !final {
		cut = runeFloor(f.pending, len(f.pending)-f.window)
		for moved := true; moved; {
			moved = false
			for _, s := range spans {
				if s.start < cut && s.end > cut {
					cut = runeFloor(f.pending, s.start)
					moved = true
				}
			}
		}
		if cut <= 0 && len(f.pending) >= forceReleaseFactor*f.window {
			cut = runeFloor(f.pending, len(f.pending)-f.window)
			for _, s := range spans {
				if s.start < cut && s.end > cut {
					cut = s.end
				}
			}
		}
		if cut <= 0 {
			f.nextEval = len(f.pending) + f.window
			return "", nil
		}
	}
	f.nextEval = 0

	var inCut []span
	for _, s := range spans {
		if s.start < cut {
			if s.end > cut {
				s.end = cut
			}
			inCut = append(inCut, s)
		}
	}
	releasedOriginal := f.pending[:cut]
	out := applyMasksFrom(releasedOriginal, inCut, f.open)
	f.released.WriteString(out)

	ctxText := f.tail + releasedOriginal
	f.pending = f.pending[cut:]
	// The match is still open when the whole held text was released and the
	// match reached its end: the next delta can continue it.
	f.open = !final && openStart >= 0 && f.pending == ""
	switch {
	case f.open && len(ctxText)-openStart <= maxOpenContextBytes:
		ctxText = ctxText[openStart:]
	case f.open:
		// The open match is too long to keep in the context. Mask the rest
		// of the channel: no later byte of it is released raw.
		f.maskRest = inCut[len(inCut)-1].label
		fallthrough
	default:
		if len(ctxText) > f.window {
			ctxText = ctxText[runeCeil(ctxText, len(ctxText)-f.window):]
		}
	}
	f.tail = ctxText
	return out, nil
}

// runeFloor moves i back to the start of the rune it falls in.
func runeFloor(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeCeil moves i forward to the start of the next rune.
func runeCeil(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

package agentproposal

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The runtime lower-cases a tool key with Rust's str::to_lowercase
// (libs/rust/agent-runtime tool_envelope.rs ToolKey, each half trimmed and
// lowered on its own), and the proposal's spec digest must name the same key
// (#1861). Go's strings.ToLower differs from it in two ways:
//
//   - a capital sigma at the end of a word lowers to the final sigma (ς,
//     U+03C2) in Rust and to σ (U+03C3) in Go (Unicode Final_Sigma);
//   - capital I with dot above (U+0130) lowers to "i" and a combining dot
//     above (U+0069 U+0307) in Rust, to a plain "i" in Go (SpecialCasing).
//
// Every other character lowers by the simple Unicode mapping on both sides.
// A character newer than Go's Unicode tables could still differ; real tool
// keys are toolkit and tool identifiers, ASCII today.

const (
	capitalSigma      = 'Σ'
	smallSigma        = 'σ'
	finalSigma        = 'ς'
	capitalIWithDot   = 'İ'
	combiningDotAbove = '̇'
)

// RuntimeLower is s lower-cased exactly as Rust's str::to_lowercase does.
func RuntimeLower(s string) string {
	if !strings.ContainsAny(s, string(capitalSigma)+string(capitalIWithDot)) {
		return strings.ToLower(s)
	}
	var out strings.Builder
	out.Grow(len(s) + 2)
	for index, r := range s {
		switch r {
		case capitalSigma:
			if casedBefore(s[:index]) && !casedAfter(s[index+utf8.RuneLen(r):]) {
				out.WriteRune(finalSigma)
			} else {
				out.WriteRune(smallSigma)
			}
		case capitalIWithDot:
			out.WriteRune('i')
			out.WriteRune(combiningDotAbove)
		default:
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

// runtimeToolKey is a tool key as the runtime's RequestedTool::canonical
// names it: each half of "toolkit/tool" trimmed and lowered on its own.
func runtimeToolKey(key string) string {
	toolkit, tool, found := strings.Cut(key, "/")
	if !found {
		return RuntimeLower(key)
	}
	return RuntimeLower(strings.TrimSpace(toolkit)) + "/" + RuntimeLower(strings.TrimSpace(tool))
}

// casedBefore answers Rust's case_ignorable_then_cased over the text before
// a sigma, read backwards: the first character that is not case-ignorable
// is cased.
func casedBefore(text string) bool {
	for len(text) > 0 {
		r, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
		if caseIgnorable(r) {
			continue
		}
		return cased(r)
	}
	return false
}

// casedAfter is the same over the text after it, read forwards.
func casedAfter(text string) bool {
	for _, r := range text {
		if caseIgnorable(r) {
			continue
		}
		return cased(r)
	}
	return false
}

// cased is the Unicode derived property Cased: Lowercase, Uppercase or
// titlecase.
func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// caseIgnorable is the Unicode derived property Case_Ignorable: Mn, Me, Cf,
// Lm and Sk, and Word_Break MidLetter, MidNumLet and Single_Quote.
func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '·', '·', '՟', '״', '‘', '’', '․', '‧',
		'︓', '﹒', '﹕', '＇', '．', '：':
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

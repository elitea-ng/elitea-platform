package guardrail

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Finding is one detector hit. Start and End are byte offsets into the
// content the detector read. Whole is set when the finding has no span and
// covers the whole content. A finding never carries the matched text.
type Finding struct {
	Start, End int
	Whole      bool
	Label      string
	Score      float64
}

// Env is what a detector may use at run time. The gateway builds it per
// request (internal/llmproxy/guardrail_gate.go).
type Env struct {
	// HTTPClient sends adapter requests. It must not follow redirects and must
	// refuse a private address the egress allowlist does not name.
	HTTPClient *http.Client
	// EgressAllows reports whether an adapter URL is on the egress allowlist.
	// Nil refuses every adapter call.
	EgressAllows func(url string) bool
	// ResolveSecret resolves a {{secret.NAME}} reference for an adapter's
	// auth header. Nil makes an adapter with an auth secret fail.
	ResolveSecret func(ctx context.Context, ref string) (string, error)
}

// detectorImpl is the detector interface: content in; spans, labels and
// scores out.
type detectorImpl interface {
	detect(ctx context.Context, env Env, stage, content string) ([]Finding, error)
}

// ---------------------------------------------------------------------------
// regex / dictionary detector

type regexDetector struct {
	label    string
	patterns []*regexp.Regexp
	terms    *regexp.Regexp
}

// newRegexDetector compiles `patterns` (RE2, Go regexp) and `terms` (literal
// words, matched case-insensitively at word boundaries). Both are bounded.
func newRegexDetector(obj map[string]any) (detectorImpl, error) {
	d := &regexDetector{}
	d.label, _ = obj["label"].(string)
	if d.label == "" {
		d.label = "custom"
	}
	if !ValidLabel(d.label) {
		return nil, fmt.Errorf("label %q must match %s", d.label, labelPattern.String())
	}
	patterns, err := stringList(obj, "patterns")
	if err != nil {
		return nil, err
	}
	terms, err := stringList(obj, "terms")
	if err != nil {
		return nil, err
	}
	if len(patterns) == 0 && len(terms) == 0 {
		return nil, fmt.Errorf("a %s detector needs at least one pattern or term", TypeRegex)
	}
	if len(patterns) > MaxPatterns {
		return nil, fmt.Errorf("patterns lists %d entries; the limit is %d", len(patterns), MaxPatterns)
	}
	for i, p := range patterns {
		if len(p) > MaxPatternBytes {
			return nil, fmt.Errorf("patterns[%d] is %d bytes; the limit is %d", i, len(p), MaxPatternBytes)
		}
		re, cerr := regexp.Compile(p)
		if cerr != nil {
			return nil, fmt.Errorf("patterns[%d] is not a valid RE2 expression: %w", i, cerr)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("patterns[%d] matches the empty string, so it would match everywhere", i)
		}
		d.patterns = append(d.patterns, re)
	}
	if len(terms) > MaxTerms {
		return nil, fmt.Errorf("terms lists %d entries; the limit is %d", len(terms), MaxTerms)
	}
	total := 0
	alts := make([]string, 0, len(terms))
	for i, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, fmt.Errorf("terms[%d] is blank", i)
		}
		if len(t) > MaxTermBytes {
			return nil, fmt.Errorf("terms[%d] is %d bytes; the limit is %d", i, len(t), MaxTermBytes)
		}
		total += len(t)
		alts = append(alts, termExpr(t))
	}
	if total > MaxTermsTotalBytes {
		return nil, fmt.Errorf("terms total %d bytes; the limit is %d", total, MaxTermsTotalBytes)
	}
	if len(alts) > 0 {
		re, cerr := regexp.Compile(`(?i)(?:` + strings.Join(alts, "|") + `)`)
		if cerr != nil {
			return nil, fmt.Errorf("terms do not compile: %w", cerr)
		}
		d.terms = re
	}
	return d, nil
}

// termExpr quotes a term and anchors it at word boundaries where the term
// starts or ends with a word character.
func termExpr(t string) string {
	expr := regexp.QuoteMeta(t)
	if isWordByte(t[0]) {
		expr = `\b` + expr
	}
	if isWordByte(t[len(t)-1]) {
		expr += `\b`
	}
	return expr
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func (d *regexDetector) detect(_ context.Context, _ Env, _ string, content string) ([]Finding, error) {
	var out []Finding
	for _, re := range d.patterns {
		for _, loc := range re.FindAllStringIndex(content, -1) {
			out = append(out, Finding{Start: loc[0], End: loc[1], Label: d.label, Score: 1})
		}
	}
	if d.terms != nil {
		for _, loc := range d.terms.FindAllStringIndex(content, -1) {
			out = append(out, Finding{Start: loc[0], End: loc[1], Label: d.label, Score: 1})
		}
	}
	return out, nil
}

package agentmemory

import (
	"regexp"
	"unicode"
)

// The secret scanner, ported from the Rust runtime's
// elitea_agent_runtime::memory_tools::looks_like_secret
// (libs/rust/agent-runtime/src/memory_tools.rs). The worker runs that one
// before it calls Main; this one runs on Main's side of every write, so a
// worker that skipped its check, or a person's edit, cannot store a
// credential either. The two lists must say the same: secrets_test.go pins
// the same cases as memory_tools_tests.rs.
//
// A cheap guard against the obvious, not a scanner of record.
var secretPatterns = compileAll(
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`,
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
	`\bgh[pousr]_[A-Za-z0-9]{30,}`,
	`\bgithub_pat_[A-Za-z0-9_]{20,}`,
	`\bxox[abprs]-[A-Za-z0-9-]{10,}`,
	`\bsk-[A-Za-z0-9_-]{20,}`,
	`\bAIza[0-9A-Za-z_-]{30,}`,
	`\beln(?:at|rt|ac)_[A-Za-z0-9_-]{16,}`,
	`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`,
	`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`,
)

// secretAssignment is a credential-named key and the value assigned to it.
// Only a token-like value makes it a secret: "api_key: stored in the team
// vault" names where a key lives and is a fact worth keeping.
var secretAssignment = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|private[_-]?key)["']?\s*[:=]\s*["']?([^\s"']+)`)

func compileAll(patterns ...string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		compiled[i] = regexp.MustCompile(pattern)
	}
	return compiled
}

// LooksLikeSecret reports whether text looks like it holds a credential.
func LooksLikeSecret(text string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	for _, match := range secretAssignment.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 && tokenLike(match[1]) {
			return true
		}
	}
	return false
}

// tokenLike: at least 6 characters with a digit or a symbol, or at least 20
// of anything. A plain word ("stored", "rotated") is not one.
func tokenLike(value string) bool {
	length, mixed := 0, false
	for _, r := range value {
		length++
		if unicode.IsDigit(r) && r < unicode.MaxASCII {
			mixed = true
		}
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !isSeparator(r) {
			mixed = true
		}
	}
	return (length >= 6 && mixed) || length >= 20
}

func isSeparator(r rune) bool {
	switch r {
	case '-', '_', '.', ',', ';':
		return true
	}
	return false
}

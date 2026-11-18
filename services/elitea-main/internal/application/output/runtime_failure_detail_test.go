package output

import (
	"strings"
	"testing"
)

// #2227: only the Code failure carries a program's output, so only its safe
// message may exceed the fixed-sentence bound, and only as the exact registered
// sentence, the exact separator and a clean, bounded detail.
func TestOnlyTheCodeFailureMayCarryAProgramOutputTail(t *testing.T) {
	carried := CodeFailureSentence + CodeFailureDetailSeparator + "Error: boom\n    at main.js:1" + strings.Repeat(" y", 150)
	long := strings.Repeat("x", MaxRuntimeFailureSafeMessageBytes+1)
	for _, tc := range []struct {
		name    string
		code    string
		message string
		ok      bool
	}{
		{"a fixed sentence", "INTERNAL", "The runtime operation failed.", true},
		{"the Code failure with output", CodeFailureCode, carried, true},
		{"another code with the same text", "INTERNAL", carried, false},
		{"a long text under the Code failure", CodeFailureCode, long, false},
		{"another sentence, then output", CodeFailureCode, "Another sentence." + CodeFailureDetailSeparator + long, false},
		{"output without the separator", CodeFailureCode, CodeFailureSentence + " " + long, false},
		{"output that is too long", CodeFailureCode, CodeFailureSentence + CodeFailureDetailSeparator + strings.Repeat("x", MaxCodeFailureDetailRunes+1), false},
		{"output with a control character", CodeFailureCode, CodeFailureSentence + CodeFailureDetailSeparator + strings.Repeat("x", 300) + "\x1b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeMessageAdmitted(tc.code, tc.message); got != tc.ok {
				t.Fatalf("SafeMessageAdmitted(%s, %d bytes) = %v, want %v", tc.code, len(tc.message), got, tc.ok)
			}
		})
	}
}

func TestCodeFailureDetailIsBoundedCleanText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail string
		ok     bool
	}{
		{"stack", "Error: boom\n    at main.js:1:7", true},
		{"tabs and newlines", "a\tb\nc", true},
		{"the bound in characters", strings.Repeat("é", MaxCodeFailureDetailRunes), true},
		{"over the bound", strings.Repeat("x", MaxCodeFailureDetailRunes+1), false},
		{"empty", "", false},
		{"blank", " \n\t", false},
		{"escape", "a\x1b[31m", false},
		{"nul", "a\x00", false},
		{"carriage return", "a\rb", false},
		{"invalid utf-8", "\xff", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidCodeFailureDetail(tc.detail); got != tc.ok {
				t.Fatalf("ValidCodeFailureDetail(%q) = %v, want %v", tc.detail, got, tc.ok)
			}
		})
	}
}

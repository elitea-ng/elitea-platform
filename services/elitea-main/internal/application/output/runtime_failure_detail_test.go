package output

import (
	"strings"
	"testing"
)

// #2227: only the Code failure carries a program's output, so only its safe
// message may exceed the fixed-sentence bound, and by exactly the separator
// and the bounded detail.
func TestOnlyTheCodeFailureMayCarryAProgramOutputTail(t *testing.T) {
	if got := MaxRuntimeFailureSafeMessageBytesFor("INTERNAL"); got != MaxRuntimeFailureSafeMessageBytes {
		t.Fatalf("a fixed-sentence failure kept its bound? got %d", got)
	}
	code := MaxRuntimeFailureSafeMessageBytesFor(CodeFailureCode)
	if code <= MaxRuntimeFailureSafeMessageBytes {
		t.Fatalf("the Code failure cannot carry its program output: %d", code)
	}
	// The registered sentence and the worst case detail (four bytes a character) fit.
	if want := MaxRuntimeFailureSafeMessageBytes + len(CodeFailureDetailSeparator) + 4*MaxCodeFailureDetailRunes; code != want {
		t.Fatalf("the Code failure bound is %d, want %d", code, want)
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

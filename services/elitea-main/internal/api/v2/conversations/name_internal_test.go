package conversations

import (
	"strings"
	"testing"
)

func TestCleanGeneratedNameKeepsOnlyATitle(t *testing.T) {
	for answer, want := range map[string]string{
		"Gateway TLS rotation":                     "Gateway TLS rotation",
		"\n\n**Title:** \"Gateway TLS rotation.\"": "Gateway TLS rotation",
		"# Gateway   TLS\trotation\nsecond line":   "Gateway TLS rotation",
		"«Ротация сертификата»":                    "Ротация сертификата",
		"   ": "",
	} {
		if got := cleanGeneratedName(answer); got != want {
			t.Errorf("cleanGeneratedName(%q) = %q, want %q", answer, got, want)
		}
	}
	long := strings.Repeat("word ", 40)
	if got := cleanGeneratedName(long); len([]rune(got)) > MaxGeneratedNameLength {
		t.Errorf("a long answer kept %d runes", len([]rune(got)))
	}
}

func TestFirstExchangeNeedsAQuestion(t *testing.T) {
	if got := firstExchange([]Message{{Role: "assistant", Content: "Hello"}}); got != "" {
		t.Errorf("an answer alone: %q", got)
	}
	got := firstExchange([]Message{{Role: "user", Content: strings.Repeat("a", nameExcerptRunes+10)}})
	if !strings.Contains(got, "…") || strings.Contains(got, "<answer>") {
		t.Errorf("a long question alone: %q", got[len(got)-40:])
	}
}

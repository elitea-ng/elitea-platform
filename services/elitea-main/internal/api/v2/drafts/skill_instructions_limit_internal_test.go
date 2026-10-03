package drafts

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSkillDraftKeepsInstructionsUpToTheSharedLimit pins #6744 part 1 for
// the draft generator. It cut a generated draft at 5 000 characters, so an
// edit of a longer saved skill came back silently shortened.
func TestSkillDraftKeepsInstructionsUpToTheSharedLimit(t *testing.T) {
	long := strings.Repeat("ж", 6000)
	if got := truncate(long, skillInstructionsMaxLength); got != long {
		t.Fatalf("a 6000-character draft was cut to %d characters", utf8.RuneCountInString(got))
	}
	over := strings.Repeat("a", skillInstructionsMaxLength+10)
	if got := utf8.RuneCountInString(truncate(over, skillInstructionsMaxLength)); got != 50000 {
		t.Fatalf("an over-limit draft kept %d characters, want 50000", got)
	}
}

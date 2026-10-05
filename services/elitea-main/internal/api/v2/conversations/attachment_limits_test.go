package conversations

import (
	"fmt"
	"testing"
)

// TestCurrentAttachmentLimitsFollowTheEnvironment pins that the policy a
// client is told is the one the upload enforces: the same variables, the same
// defaults, read the same way.
func TestCurrentAttachmentLimitsFollowTheEnvironment(t *testing.T) {
	defaults := CurrentAttachmentLimits(nil)
	if defaults.MaxFileBytes != 150<<20 || defaults.MaxTotalBytes != 150<<20 || defaults.MaxImageBytes != 3<<20 {
		t.Fatalf("defaults = %+v, want 150/150/3 MiB", defaults)
	}
	if defaults.ChunkBytes != 5<<20 || defaults.MaxFiles != 10 || defaults.MaxExtractBytes != 25<<20 {
		t.Fatalf("defaults = %+v, want 5 MiB chunks, 10 files, 25 MiB extraction", defaults)
	}
	if fmt.Sprint(defaults.InlineImageFormats) != "[.gif .jpeg .jpg .png .webp]" || defaults.InlineImageMaxBytes <= 0 {
		t.Fatalf("inline image policy = %v / %d", defaults.InlineImageFormats, defaults.InlineImageMaxBytes)
	}
	if len(defaults.AcceptedExtensions) != len(DefaultAttachmentExtensions) {
		t.Fatalf("default extensions = %v", defaults.AcceptedExtensions)
	}

	t.Setenv("ARTIFACT_ATTACHMENT_MAX_FILE_MB", "7")
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_IMAGE_MB", "2")
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_TOTAL_MB", "9")
	overridden := CurrentAttachmentLimits([]string{"PDF", ".md", "md", " .Txt "})
	if overridden.MaxFileBytes != 7<<20 || overridden.MaxImageBytes != 2<<20 || overridden.MaxTotalBytes != 9<<20 {
		t.Fatalf("env overrides = %+v, want 7/2/9 MiB", overridden)
	}
	if got := fmt.Sprint(overridden.AcceptedExtensions); got != "[.md .pdf .txt]" {
		t.Fatalf("extensions = %s, want normalized, sorted, de-duplicated [.md .pdf .txt]", got)
	}

	// A value the upload path ignores is ignored here too.
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_FILE_MB", "not-a-number")
	if got := CurrentAttachmentLimits(nil).MaxFileBytes; got != 150<<20 {
		t.Fatalf("an unparseable override gave %d, want the 150 MiB default", got)
	}
}

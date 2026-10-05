package conversations

import (
	"sort"
	"strings"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// ComposerMaxAttachments is how many files one message carries in the
// composer (apps/elitea-web/src/shared/lib/attachments.ts,
// ATTACHMENT_LIMITS.MAX_ATTACHMENTS). Admission itself accepts more
// (maxCurrentTurnAttachments); this is the product limit a client shows.
const ComposerMaxAttachments = 10

// DefaultAttachmentExtensions is what a client may attach when the
// deployment does not serve the SDK's loader catalogue
// (ELITEA_INDEX_TYPES_ENABLED unset): the web composer's own fallback list
// (apps/elitea-web/src/entities/attachment/api/allowedTypes.ts,
// DEFAULT_ATTACHMENT_EXTENSIONS).
var DefaultAttachmentExtensions = []string{
	".csv", ".doc", ".docx", ".gif", ".htm", ".html", ".jpeg", ".jpg", ".json",
	".md", ".pdf", ".png", ".pptx", ".svg", ".txt", ".webp", ".xls", ".xlsx",
	".xml", ".yaml", ".yml",
}

// AttachmentLimits is the deployment-wide attachment policy a client is told
// (client contract 1.1). The values are the ones the upload and admission
// paths enforce, read from the same environment at the same defaults.
type AttachmentLimits struct {
	MaxFiles             int
	MaxTotalBytes        int64
	MaxFileBytes         int64
	MaxImageBytes        int64
	ChunkBytes           int64
	AcceptedExtensions   []string
	MaxExtractBytes      int64
	InlineImageMaxBytes  int64
	InlineImageFormats   []string
	InlineImageDownscale bool
}

// CurrentAttachmentLimits answers the limits for this process's environment.
// extensions is the accepted list (nil or empty takes the default list); it
// is normalized to lower-case, leading-dot, sorted and de-duplicated.
func CurrentAttachmentLimits(extensions []string) AttachmentLimits {
	if len(extensions) == 0 {
		extensions = DefaultAttachmentExtensions
	}
	return AttachmentLimits{
		MaxFiles:             ComposerMaxAttachments,
		MaxTotalBytes:        attachmentMaxTotalBytes(),
		MaxFileBytes:         attachmentMaxFileBytes(),
		MaxImageBytes:        attachmentMaxImageBytes(),
		ChunkBytes:           attachmentMaxChunkBytes,
		AcceptedExtensions:   normalizeExtensions(extensions),
		MaxExtractBytes:      extract.DefaultLimits().MaxInputBytes,
		InlineImageMaxBytes:  agentexecutionapp.InlineAttachmentImageMaxBytes(),
		InlineImageFormats:   agentexecutionapp.InlineAttachmentImageFormats(),
		InlineImageDownscale: agentexecutionapp.InlineAttachmentImageDownscale,
	}
}

func normalizeExtensions(extensions []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(extensions))
	for _, extension := range extensions {
		normalized := strings.ToLower(strings.TrimSpace(extension))
		if normalized == "" || normalized == "." {
			continue
		}
		if !strings.HasPrefix(normalized, ".") {
			normalized = "." + normalized
		}
		if !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	sort.Strings(out)
	return out
}

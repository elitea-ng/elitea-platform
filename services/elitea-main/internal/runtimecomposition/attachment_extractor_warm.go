package runtimecomposition

import (
	"context"
	"log/slog"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// attachmentExtractorWarmTimeout bounds the start-up compilation of the PDF
// engine. A failure is logged and changes nothing else: the first PDF then
// compiles the module itself.
const attachmentExtractorWarmTimeout = 2 * time.Minute

// warmAttachmentExtractor compiles the PDF engine in the background.
func warmAttachmentExtractor(extractor *extract.Extractor, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachmentExtractorWarmTimeout)
	defer cancel()
	started := time.Now()
	if err := extractor.Warm(ctx); err != nil {
		logger.Warn("attachment pdf engine warm-up failed; the first pdf compiles it", "error", err)
		return
	}
	logger.Info("attachment pdf engine compiled", "duration_ms", time.Since(started).Milliseconds())
}

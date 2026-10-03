package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

// TestMain compiles PDFium once, before any test runs, as the composition
// root does with Extractor.Warm at start-up. The attachment tests read PDFs
// under the production extraction deadline; a compilation inside it, under
// -race on a CI runner, turned their outcome into "timeout".
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	err := extract.New(extract.DefaultLimits()).Warm(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warm the PDF engine: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

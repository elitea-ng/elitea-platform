package extract

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain compiles PDFium once, before any test runs. Every PDF test has the
// production 60-second extraction deadline; under -race on a CI runner, a
// compilation inside that deadline (or a dozen parallel ones) used up the
// whole budget and the tests answered "timeout" instead of their outcome.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	err := New(DefaultLimits()).Warm(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warm the PDF engine: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

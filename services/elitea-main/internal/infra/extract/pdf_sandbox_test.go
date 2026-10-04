package extract

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
)

// openByPath opens a PDF through a FILE PATH inside the WebAssembly module,
// the way a parser bug or a refactor could, and reports whether it worked.
func openByPath(t *testing.T, config webassembly.Config, file string) bool {
	t.Helper()
	pool, err := webassembly.Init(config)
	require.NoError(t, err)
	defer func() { _ = pool.Close() }()
	instance, err := pool.GetInstanceWithContext(config.Context)
	require.NoError(t, err)
	defer func() { _ = instance.Close() }()
	opened, err := instance.OpenDocument(&requests.OpenDocument{FilePath: &file})
	if err != nil {
		return false
	}
	_, _ = instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: opened.Document})
	return true
}

// THE SANDBOX HAS NO HOST FILE SYSTEM. go-pdfium mounts the host root "/"
// by default; poolConfig must replace that with an empty mount. The test
// writes a valid PDF to disk and opens it BY PATH through the production
// configuration, which must fail, and through the same configuration with
// the root mounted, which must succeed. The second half proves the first
// half can fail: without it, a broken open would look like a sandbox.
func TestPDFEngineSandboxCannotReadHostFiles(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "on-host.pdf")
	require.NoError(t, os.WriteFile(file, onePagePDF([]string{"HOSTFILETOKEN"}), 0o600))
	absolute, err := filepath.Abs(file)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	engine := newPDFEngine(DefaultLimits().PDFMemoryLimitPages)

	production := engine.poolConfig(ctx)
	require.NotNil(t, production.FSConfig, "a nil FSConfig makes go-pdfium mount the host root")
	require.False(t, openByPath(t, production, absolute), "the production sandbox reached a host file")

	mounted := engine.poolConfig(ctx)
	mounted.FSConfig = wazero.NewFSConfig().WithDirMount("/", "/")
	require.True(t, openByPath(t, mounted, absolute), "control: with the root mounted the same open must work")
}

func TestPDFEngineWarmCompilesOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	extractor := New(DefaultLimits())
	require.NoError(t, extractor.Warm(ctx))
	started := time.Now()
	doc, err := extractor.Extract(ctx, onePagePDF([]string{"WARMTOKEN"}))
	require.NoError(t, err)
	require.Contains(t, doc.Text, "WARMTOKEN")
	require.Less(t, time.Since(started), 10*time.Second, "a warm engine reads one page well inside the route's wait")
	var none *Extractor
	require.ErrorIs(t, none.Warm(ctx), ErrUnavailable)
}

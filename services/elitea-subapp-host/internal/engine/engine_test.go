package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestExportBoundMirrorsTheEngine pins MaxExportDocumentBytes to the
// Inventory engine's MAX_EXPORT_DOCUMENT_BYTES, read from the Rust source,
// and keeps MaxStreamLineBytes above it with headroom: an export the engine
// admits must fit in one stream line this host reads.
func TestExportBoundMirrorsTheEngine(t *testing.T) {
	path := filepath.Join("..", "..", "..", "elitea-inventory-engine", "src", "lib.rs")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	match := regexp.MustCompile(`pub const MAX_EXPORT_DOCUMENT_BYTES: usize = (\d+) << (\d+);`).FindSubmatch(raw)
	if match == nil {
		t.Fatalf("%s declares no MAX_EXPORT_DOCUMENT_BYTES of the form `N << M`", path)
	}
	value, _ := strconv.Atoi(string(match[1]))
	shift, _ := strconv.Atoi(string(match[2]))
	if rust := value << shift; rust != MaxExportDocumentBytes {
		t.Fatalf("MAX_EXPORT_DOCUMENT_BYTES is %d in Rust, MaxExportDocumentBytes is %d here", rust, MaxExportDocumentBytes)
	}
	if MaxStreamLineBytes < MaxExportDocumentBytes+streamLineOverhead {
		t.Fatalf("MaxStreamLineBytes (%d) leaves less than %d bytes above the export bound (%d)",
			MaxStreamLineBytes, streamLineOverhead, MaxExportDocumentBytes)
	}
}

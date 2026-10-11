package engine

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
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

// An engine of an older release answers HTTP 400 "Unknown tool" for a tool it
// does not serve; the host tells that apart from every other refusal.
func TestAnUnknownToolIsRecognisedAndOtherRefusalsAreNot(t *testing.T) {
	status, body := http.StatusBadRequest, `{"detail":"Unknown tool: delete_wiki_index"}`
	dir, err := os.MkdirTemp("/tmp", "eng")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "e.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	client := NewClient(socket, "DeepWiki")
	tc := spi.DetachedContext("test")
	_, err = client.Invoke(t.Context(), "delete_wiki_index", map[string]any{}, tc)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("an unknown tool was not recognised: %v", err)
	}
	status, body = http.StatusBadRequest, `{"detail":"arguments must be an object"}`
	if _, err = client.Invoke(t.Context(), "x", map[string]any{}, tc); err == nil || errors.Is(err, ErrUnknownTool) {
		t.Fatalf("another 400 was taken for an unknown tool: %v", err)
	}
	status, body = http.StatusInternalServerError, `Unknown tool`
	if _, err = client.Invoke(t.Context(), "x", map[string]any{}, tc); err == nil || errors.Is(err, ErrUnknownTool) {
		t.Fatalf("a 500 was taken for an unknown tool: %v", err)
	}
}

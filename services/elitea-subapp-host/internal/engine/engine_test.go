package engine

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	enginev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/engine/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/engine/enginetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
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

// refusingEngine refuses every Invoke with the status it holds.
type refusingEngine struct {
	enginev1.UnimplementedEngineServiceServer
	refusal error
}

func (r *refusingEngine) Invoke(*enginev1.InvokeRequest, enginev1.EngineService_InvokeServer) error {
	return r.refusal
}

// An engine of an older release refuses a tool it does not serve with
// INVALID_ARGUMENT "Unknown tool: …"; the host tells that apart from every
// other refusal.
func TestAnUnknownToolIsRecognisedAndOtherRefusalsAreNot(t *testing.T) {
	engine := &refusingEngine{refusal: status.Error(codes.InvalidArgument, "Unknown tool: delete_wiki_index")}
	client := NewClient(enginetest.Serve(t, engine), "DeepWiki")
	tc := spi.DetachedContext("test")
	_, err := client.Invoke(t.Context(), "delete_wiki_index", map[string]any{}, tc)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("an unknown tool was not recognised: %v", err)
	}
	engine.refusal = status.Error(codes.InvalidArgument, "arguments must be an object")
	if _, err = client.Invoke(t.Context(), "x", map[string]any{}, tc); err == nil || errors.Is(err, ErrUnknownTool) {
		t.Fatalf("another refusal was taken for an unknown tool: %v", err)
	}
	engine.refusal = status.Error(codes.Internal, "Unknown tool")
	if _, err = client.Invoke(t.Context(), "x", map[string]any{}, tc); err == nil || errors.Is(err, ErrUnknownTool) {
		t.Fatalf("an internal error was taken for an unknown tool: %v", err)
	}
}

// An engine that is not there is a runtime error naming the socket, not a
// hang: the dial fails fast.
func TestAnAbsentEngineIsNotReachable(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "absent.sock"), "DeepWiki")
	_, err := client.Invoke(t.Context(), "ask", map[string]any{}, spi.DetachedContext("test"))
	if err == nil || !regexp.MustCompile(`The DeepWiki engine is not reachable at .*absent\.sock`).MatchString(err.Error()) {
		t.Fatalf("%v", err)
	}
}

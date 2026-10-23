package deepwiki_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	deepwiki "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
)

// The DeepWiki hop clears the per-connection write deadline before streaming.
//
// WHY A SEPARATE TEST FROM providerhost/hop's OWN. That package's test proves
// ClearWriteDeadline works in isolation. It cannot prove this hop CALLS it —
// and this hop did not, for as long as it has existed, while the /llm hop
// always did. A helper nobody calls is the shape this repository's wiring
// tests exist for.
//
// It is LATENT today: cmd/elitea-main's server sets ReadHeaderTimeout and
// IdleTimeout and no WriteTimeout, so nothing currently truncates a long
// response. This test is what makes adding one later safe — without it, a
// perfectly reasonable "add a WriteTimeout" change would cut generation
// results off mid-stream on this hop only, with no error anywhere and a
// client that just sees the stream stop.
type deadlineWriter struct {
	http.ResponseWriter
	cleared bool
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		d.cleared = true
	}
	return nil
}

func TestTheHopClearsTheWriteDeadlineBeforeStreaming(t *testing.T) {
	_, cfg := provider(t)

	proxy, err := deepwiki.NewProxy(cfg, nil)
	if err != nil {
		t.Fatalf("building the proxy: %v", err)
	}

	writer := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodGet, "/api/v2/deepwiki/slots/1", nil)

	proxy.Forward(writer, request, "/slots", "1", "7")

	if !writer.cleared {
		t.Fatal("Forward did not clear the write deadline. A WriteTimeout on the " +
			"server would truncate a generation result mid-stream, with nothing logged.")
	}
}

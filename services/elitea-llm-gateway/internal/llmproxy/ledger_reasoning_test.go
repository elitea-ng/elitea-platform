package llmproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// The usage ledger row takes the streamed request's settled count from the
// detached billing context, else the unary request log's (ADR-0038).
func TestLedgerReasoningTokensReadsTheBillingContextThenTheRequestLog(t *testing.T) {
	if got := ledgerReasoningTokens(context.Background()); got != 0 {
		t.Fatalf("nothing recorded: %d", got)
	}
	if got := ledgerReasoningTokens(withLedgerReasoningTokens(context.Background(), 77)); got != 77 {
		t.Fatalf("billing context: %d, want 77", got)
	}
	var got int64
	rec := requestlog.New(&verdictSink{}, nil)
	serve := func(_ http.ResponseWriter, r *http.Request) {
		requestlog.FromContext(r.Context()).SetReasoningTokens(31)
		got = ledgerReasoningTokens(r.Context())
	}
	requestlog.Middleware(rec)(http.HandlerFunc(serve)).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec.Stop(ctx)
	if got != 31 {
		t.Fatalf("request log: %d, want 31", got)
	}
}

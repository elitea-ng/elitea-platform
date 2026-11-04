package llmproxy

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func (f *fakeBudgetChecker) lastReasoning() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastDims == nil {
		return -1
	}
	return f.lastDims.ReasoningTokens
}

// ADR-0038 decision 5: a trailer that arrives during the grace drain, after
// the request log row was written, still gives the usage ledger its billed
// reasoning count.
func TestLedgerReasoningTakesATrailerRecoveredByTheDrain(t *testing.T) {
	gate := allowGate()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	trailer := chatTrailer(11, 22)
	trailer.BifrostChatResponse.Usage.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{ReasoningTokens: 9}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		accIn:    3, accOut: 4,
		trailer:      trailer,
		trailerDelay: 20 * time.Millisecond,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(newRecordingEvents()),
		WithStreamGrace(2*time.Second))

	h.Chat(&nonFlusherWriter{}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.lastReasoning(); got != 9 {
		t.Fatalf("ledger reasoning = %d, want the drained trailer's 9", got)
	}
}

// On the accumulated-usage path (no trailer at all) the ledger takes the
// reasoning count from the provider's handle, with the tokens it bills.
func TestLedgerReasoningTakesTheAccumulatedCount(t *testing.T) {
	gate := allowGate()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello")},
		accIn:    11, accOut: 22, accReasoning: 7,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(newRecordingEvents()),
		WithStreamGrace(50*time.Millisecond))

	h.Chat(&nonFlusherWriter{}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.lastReasoning(); got != 7 {
		t.Fatalf("ledger reasoning = %d, want the accumulated 7", got)
	}
}

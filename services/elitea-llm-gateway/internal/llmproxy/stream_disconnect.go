package llmproxy

// stream_disconnect.go — what the gateway does when the CALLER of a streamed
// request goes away (issue #1201, human decision 2026-11-02).
//
// Before this file, a caller disconnect went down the same path as every other
// early exit (stream_drain.go, settleEarly): the provider stream stayed alive
// for up to the stream grace so the drain could catch the usage trailer. That
// closed the "disconnect just before the end for free inference" hole, but it
// kept the model slot busy for a generation nobody reads. On a self-hosted
// model (vLLM) that is up to the full grace of wasted GPU time per Stop click.
//
// The decision: free the slot AND keep the bill. On a caller disconnect the
// gateway cancels the provider stream at once and charges an ESTIMATE:
//
//   - input: the prompt count the provider already reported on a stream chunk
//     or on its accumulated-usage handle, else the gateway's own estimate of
//     the request text (promptEstimate);
//   - output: the larger of the provider's accumulated count and the tokens
//     the gateway already relayed to the caller (relayedOutputTokens). The
//     charge is never below what the caller received.
//
// A usage trailer that arrives before the provider unwinds still wins: it is
// authoritative, and it is billed as such. The estimate is marked on the
// request-log row (error_code errorCodeUsageEstimated) and on the operator
// event budget.unbilled_stream (reason estimated_on_disconnect), so a billed
// estimate is never mistaken for a provider-reported count.
//
// The detached drain with its grace stays for the exits where the caller may
// still be connected or where the provider is the one that stopped: provider
// error, guardrail block, failed stream setup.

import (
	"encoding/json"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/account"
)

const (
	// disconnectCloseWait bounds how long the request goroutine waits for a
	// cancelled provider to close its channel. Cancellation makes bifrost close
	// the provider socket, so the close is normally immediate. The wait exists
	// so the accumulated-usage handle becomes readable (the close is its
	// happens-before edge) and so the request-log row carries the estimate.
	disconnectCloseWait = time.Second

	// estimateBytesPerToken is the text-to-token ratio of the estimate. Four
	// bytes per token is the common rule for English text on BPE tokenizers,
	// and elitea-main's attachment extraction uses the same ratio.
	estimateBytesPerToken = 4

	// lossReasonEstimatedOnDisconnect: the caller disconnected, the gateway
	// cancelled the provider, and the stream was billed from an estimate. Money
	// WAS billed; the event exists because an estimate is not a provider count.
	lossReasonEstimatedOnDisconnect = "estimated_on_disconnect"

	// drainOutcomeCancelled: the provider was cancelled on the disconnect and
	// closed its channel inside disconnectCloseWait.
	drainOutcomeCancelled = "cancelled_on_disconnect"
	// drainOutcomeCancelledSlowClose: the provider was cancelled but did not
	// close its channel inside disconnectCloseWait. A background reader drains
	// it so the provider goroutine can unwind.
	drainOutcomeCancelledSlowClose = "cancelled_on_disconnect/slow_close"

	// errorCodeUsageEstimated is the request-log error_code of a streamed row
	// whose token counts are an estimate. The row keeps status 200 (the stream
	// started), so the analytics failure views (status >= 400) do not count it.
	errorCodeUsageEstimated = "client_disconnected_usage_estimated"
)

// promptEstimateKey carries the request's prompt estimate on the
// BifrostContext. The value is a func so the text is measured only when a
// stream is cut, not on every request.
type promptEstimateKey struct{}

// setPromptEstimate attaches the prompt estimate of the request to ctx.
func setPromptEstimate(ctx *schemas.BifrostContext, estimate func() int64) {
	if ctx == nil || estimate == nil {
		return
	}
	ctx.SetValue(promptEstimateKey{}, estimate)
}

// promptEstimateFrom returns the prompt estimate attached to ctx, or nil.
func promptEstimateFrom(ctx *schemas.BifrostContext) func() int64 {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(promptEstimateKey{}).(func() int64)
	return fn
}

// tokensFromBytes converts a text length to an estimated token count, rounded
// up so a non-empty text is never zero tokens.
func tokensFromBytes(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + estimateBytesPerToken - 1) / estimateBytesPerToken
}

// textBytes sums the lengths of texts.
func textBytes(texts []*string) int64 {
	var n int64
	for _, t := range texts {
		if t != nil {
			n += int64(len(*t))
		}
	}
	return n
}

// jsonBytes is the marshalled length of v, 0 when it does not marshal.
func jsonBytes(v any) int64 {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(raw))
}

// chatPromptEstimate estimates the prompt tokens of a chat request from its
// TEXT: message content, tool results and tool definitions. Inline images and
// audio are left out on purpose: their base64 bytes are not prompt tokens, and
// counting them would over-bill by orders of magnitude.
func chatPromptEstimate(req *schemas.BifrostChatRequest) int64 {
	if req == nil {
		return 0
	}
	n := textBytes(chatRequestTexts(req))
	if req.Params != nil && len(req.Params.Tools) > 0 {
		n += jsonBytes(req.Params.Tools)
	}
	return tokensFromBytes(n)
}

// responsesPromptEstimate estimates the prompt tokens of a Responses (and
// Anthropic messages) request. See chatPromptEstimate.
func responsesPromptEstimate(req *schemas.BifrostResponsesRequest) int64 {
	if req == nil {
		return 0
	}
	n := textBytes(responsesRequestTexts(req))
	if req.Params != nil && len(req.Params.Tools) > 0 {
		n += jsonBytes(req.Params.Tools)
	}
	return tokensFromBytes(n)
}

// chunkPromptTokens reads a provider-reported prompt count from ANY usage
// block on a chunk, terminal or not. A prompt count is final as soon as the
// provider reports it (Anthropic message_start, vLLM continuous usage), so
// unlike the output count it needs no terminal-event gate.
func chunkPromptTokens(c *schemas.BifrostStreamChunk) int64 {
	if c == nil {
		return 0
	}
	if chat := c.BifrostChatResponse; chat != nil && chat.Usage != nil {
		return int64(chat.Usage.PromptTokens)
	}
	if sr := c.BifrostResponsesStreamResponse; sr != nil && sr.Response != nil && sr.Response.Usage != nil {
		return int64(sr.Response.Usage.InputTokens)
	}
	return 0
}

// relayed records output the caller received. The SSE loops call it after a
// write SUCCEEDS, so the count is what really reached the caller.
func (s *streamSettler) relayed(deltaBytes int64) {
	if deltaBytes <= 0 {
		return
	}
	s.relayedChunks++
	s.relayedBytes += deltaBytes
}

// relayedResponses records the Responses events written to the caller.
func (s *streamSettler) relayedResponses(events ...*schemas.BifrostResponsesStreamResponse) {
	for _, ev := range events {
		if ev != nil && ev.Delta != nil {
			s.relayed(int64(len(*ev.Delta)))
		}
	}
}

// relayedOutputTokens estimates the output tokens the caller received. Each
// content chunk carries at least one token, and the text carries about one
// token per estimateBytesPerToken bytes; the larger of the two is the floor.
func (s *streamSettler) relayedOutputTokens() int64 {
	return max(s.relayedChunks, tokensFromBytes(s.relayedBytes))
}

// disconnectEstimate computes the estimated usage of a stream cut on a caller
// disconnect. Provider-reported counts are preferred where they exist; the
// gateway's own estimates fill the gaps. The output is never below what was
// relayed.
func (s *streamSettler) disconnectEstimate() (in, out int64) {
	in = s.seenPrompt
	out = s.relayedOutputTokens()
	// The handle is read only after the channel closed (see chanClosed).
	if s.chanClosed && s.ctx != nil {
		if u, ok := s.ctx.Value(schemas.BifrostContextKeyStreamAccumulatedUsage).(*schemas.BifrostLLMUsage); ok && u != nil {
			in = max(in, int64(u.PromptTokens))
			// An output count at the placeholder floor reports no work done
			// (see placeholderOutputTokens).
			if c := int64(u.CompletionTokens); c > placeholderOutputTokens {
				out = max(out, c)
			}
		}
	}
	if in <= 0 && s.promptEstimate != nil {
		in = s.promptEstimate()
	}
	return max(in, 0), max(out, 0)
}

// settleDisconnect settles a stream whose caller went away. It cancels the
// provider AT ONCE, so the model slot is free, and bills the trailer if one
// already arrived, else an estimate.
func (s *streamSettler) settleDisconnect(reason string) {
	if s.settled {
		return
	}
	s.settled = true
	s.pinHolds()
	// Free the model slot: bifrost closes the provider socket on cancel.
	s.sc.cancel()

	outcome := drainOutcomeCancelled
	if s.ch != nil && !s.awaitClose(disconnectCloseWait) {
		outcome = drainOutcomeCancelledSlowClose
		s.discardRest()
	}

	if s.gotUsage {
		// The terminal trailer arrived before the provider unwound. It is
		// authoritative: bill it as a normal stream.
		s.recordLogTokens()
		s.report(reason, outcome)
		return
	}

	in, out := s.disconnectEstimate()
	if in > 0 || out > 0 {
		s.in, s.out, s.gotUsage, s.estimated = in, out, true, true
	}
	// Record on the request goroutine: the row is written when the handler
	// returns, so a later write would be dropped.
	s.recordLogTokens()
	if s.estimated {
		s.logUsage.SetError(errorCodeUsageEstimated)
	} else if s.ctx != nil {
		s.logUsage.SetCredentialOwner(account.SelectedCredentialOwner(s.ctx))
	}

	// The settle can publish to NATS (up to the 150 ms flush bound). The
	// caller is gone, but the request goroutine still holds the row, so hand
	// the settle to a tracked goroutine as settleClean does.
	if !s.h.trackDrain() {
		s.report(reason, outcome)
		return
	}
	go func() {
		defer s.h.drainWg.Done()
		s.report(reason, outcome)
	}()
}

// awaitClose reads the cancelled provider's channel until it closes or wait
// elapses. Chunks still in flight are observed: a usage trailer among them is
// authoritative. It returns true when the channel closed.
func (s *streamSettler) awaitClose(wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case c, ok := <-s.ch:
			if !ok {
				s.chanClosed = true
				return true
			}
			s.observe(c)
		case <-t.C:
			return false
		}
	}
}

// discardRest reads and drops the rest of a cancelled provider's channel on a
// tracked goroutine, so the provider's pending send unblocks and it can
// unwind. It does not touch the settler's counters: the settle has already
// read them, and a write here would race it.
func (s *streamSettler) discardRest() {
	ch := s.ch
	tracked := s.h.trackDrain()
	go func() {
		if tracked {
			defer s.h.drainWg.Done()
		}
		hard := time.NewTimer(drainHardTimeout)
		defer hard.Stop()
		closingC := s.h.drainClosing
		if s.h.drainsClosing.Load() != 0 {
			hard.Reset(drainShutdownTimeout)
			closingC = nil
		}
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-closingC:
				// Shutdown started: a wedged provider must not hold the
				// pod's termination grace for the full backstop.
				closingC = nil
				hard.Reset(drainShutdownTimeout)
			case <-hard.C:
				s.h.logger.Warn("stream disconnect: provider channel never closed after cancellation; abandoning",
					"loop", s.loop, "provider", s.provider, "model", s.model, "project_id", s.projectID)
				return
			}
		}
	}()
}

// reportEstimatedBill records a stream that WAS billed from an estimate. It
// rides budget.unbilled_stream with its own reason, so an operator sees how
// often the bill is an estimate and how large it is.
func (s *streamSettler) reportEstimatedBill(reason, outcome string) {
	s.h.publishStreamLossEvent(unbilledStreamPayload{
		ProjectID:             s.projectID,
		Provider:              s.provider,
		Model:                 s.model,
		Reason:                lossReasonEstimatedOnDisconnect,
		DrainOutcome:          outcome,
		ObservedOutputBytes:   s.observedOutBytes,
		ExitReason:            reason,
		EstimatedInputTokens:  s.in,
		EstimatedOutputTokens: s.out,
	})
}

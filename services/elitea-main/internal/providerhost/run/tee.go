package run

// Observing the terminal payload a facade proxies.
//
// A FACADE SEES AN ANSWER EXACTLY ONCE: when the browser drains it. Every
// provider route here is a proxy, so there is no other moment — the invoke
// returns an id, the events are read-once, and the result arrives on
// whichever poll the run happened to finish before. A facade that wants to
// record what a provider produced has to read it in flight or not at all.
//
// THE ANSWER'S OWN FRAGMENTS ARE NOT THAT MOMENT. Since issue #701 the
// events carry the answer as it is written, and a transcript assembled from
// them would depend on the browser having polled often enough to see every
// one. The terminal body carries the whole answer either way, so that is
// what is recorded.
//
// THE READ IS A COPY, NOT A CONSUMPTION. providerhost/proxy buffers the
// response in its ModifyResponse hook and puts the bytes back in front of the
// stream, so the caller receives exactly what the provider sent, byte for
// byte, whether or not anybody is watching. It is done there rather than by
// wrapping the ResponseWriter for two reasons written out in that package: a
// wrapper has to forward Flush or a streaming response stops streaming, and it
// puts a Write on a caller-influenced body, which CodeQL reads as a
// reflected-XSS sink.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
)

// Forwarder is the hop. An ALIAS rather than a defined type, so a value of it
// is assignable to routes.Forwarder and material.Forwarder without a
// conversion at every call site — three packages naming the same signature is
// already one too many.
type Forwarder = func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string)

// Observed is one terminal poll a facade watched pass through it, with the
// identity the hop was signed for. The ids are the PATH's, resolved by the
// route table, so an observer never has to re-parse them.
type Observed struct {
	Terminal
	ProjectID string
	UserID    string
}

// Recorder is told about one finished run. It is called on the request's own
// goroutine AFTER the response has been written, so it delays nothing the
// caller is waiting on.
type Recorder func(ctx context.Context, observed Observed)

// DefaultCaptureLimit bounds the buffered payload. A completed deep-research
// result is the largest thing on this route and is measured in tens of
// kilobytes; a megabyte is well past it.
const DefaultCaptureLimit = 1 << 20

// Tee returns a hop that forwards through inner and hands every TERMINAL poll
// to record.
//
// Nothing about the response the caller receives depends on whether recording
// succeeds, or on whether the body could be read at all. A payload larger than
// the limit is reported as unreadable rather than as a truncated answer: half
// a JSON document is not a shorter one, and recording a prefix would put a cut
// answer in a transcript that is then read back as the whole of it.
//
// A nil record returns inner unchanged, so a deployment that keeps no record
// pays nothing — not even the buffering.
func Tee(inner Forwarder, limit int, logger *slog.Logger, record Recorder) Forwarder {
	if record == nil || inner == nil {
		return inner
	}
	if limit <= 0 {
		limit = DefaultCaptureLimit
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string) {
		outcome := &proxy.Outcome{CaptureLimit: limit}
		inner(w, r.WithContext(proxy.WithOutcome(r.Context(), outcome)), providerPath, projectID, userID)

		if outcome.Status != http.StatusOK || len(outcome.Body) == 0 {
			return
		}
		if outcome.Truncated {
			logger.Warn("provider poll exceeded the capture limit; it is not recorded",
				"path", providerPath, "limit", limit)
			return
		}
		terminal, ok := TerminalOf(outcome.Body)
		if !ok {
			return
		}
		// WithoutCancel: the browser has its answer, so its context may
		// already be cancelled — and the write that makes that answer durable
		// must not be the casualty of the request that delivered it.
		record(context.WithoutCancel(r.Context()),
			Observed{Terminal: terminal, ProjectID: projectID, UserID: userID})
	}
}

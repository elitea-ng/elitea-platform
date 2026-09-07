// Package api mounts the gateway's /llm chi routes. The route ordering is the
// dialect discriminator: the Anthropic surface shares the /llm/v1/ prefix with
// OpenAI callers (ChatAnthropic posts to {base}/llm/v1/messages), so the exact
// /llm/v1/messages route MUST be registered before the /llm/v1/* OpenAI
// catch-all or Anthropic callers are misrouted (design §3.1, §3.2). This
// ordering is a Build gate covered by regression tests.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// NewRouter builds the chi router for the /llm surface, wiring each route to
// the given handler. The routes are mounted under the full inbound path
// (/llm/v1/...) because elitea-main's reverse proxy preserves the path verbatim
// (no StripPrefix); the gateway sees /llm/v1/... exactly as the client sent it.
func NewRouter(h *llmproxy.Handler) http.Handler {
	return NewRouterWithLog(h, nil)
}

// NewRouterWithLog is NewRouter with the per-request log attached.
//
// The recorder is applied as MIDDLEWARE on the router root rather than inside
// each handler, which is what makes the log complete: every route below is
// covered without being named, a route added later is covered without anyone
// remembering, and the requests that never reach a handler — NotFound,
// MethodNotAllowed, anything refused before dispatch — are covered too. Those
// are frequently the ones an operator is looking for.
//
// A nil recorder is a pass-through, so a gateway with no database (the
// supported bootstrap posture) pays nothing.
func NewRouterWithLog(h *llmproxy.Handler, recorder *requestlog.Recorder) http.Handler {
	r := chi.NewRouter()
	r.Use(requestlog.Middleware(recorder))
	// ISSUE #164: hop-marker detection, mounted on the ROOT for the same
	// reason the request log is — every route below is covered without being
	// named, a route added later is covered without anyone remembering, and
	// the paths that never reach a handler (NotFound, MethodNotAllowed, the
	// realtime upgrade) are covered too. A routing loop does not agree to use
	// only the routes somebody remembered to annotate.
	//
	// It runs AFTER the request log so a refused re-entry is still recorded,
	// and BEFORE every handler so a loop is contained before dispatch. An
	// unarmed marker (no GATEWAY_HOP_SECRET) makes it a pass-through.
	r.Use(h.HopGuard)
	r.NotFound(h.NotFound)
	r.MethodNotAllowed(h.MethodNotAllowed)

	r.Route("/llm/v1", func(r chi.Router) {
		// Anthropic dialect — exact routes registered BEFORE the OpenAI
		// catch-all. count_tokens is synchronous (non-SSE); any other
		// /messages/{suffix} is 404 rather than misrouted.
		r.Post("/messages", h.Messages)
		r.Post("/messages/count_tokens", h.CountTokens)
		r.Post("/messages/*", h.MessagesSubPath)

		// OpenAI dialect — multipart image routes are decoded by the gateway
		// itself (net/http multipart), so they are mounted explicitly rather
		// than falling through the JSON catch-all.
		r.Post("/images/edits", h.ImageEdit)
		r.Post("/images/variations", h.ImageVariation)
		r.Post("/images/generations", h.ImageGeneration)

		// OpenAI dialect — audio. transcriptions/translations carry a
		// multipart body and are decoded by the gateway itself, like the
		// image edit/variation routes above. speech carries JSON and answers
		// raw audio bytes (issue #323, llmproxy/audio.go).
		r.Post("/audio/speech", h.Speech)
		r.Post("/audio/transcriptions", h.Transcription)
		r.Post("/audio/translations", h.Transcription)

		// OpenAI dialect — explicit JSON routes.
		r.Post("/chat/completions", h.Chat)
		r.Post("/completions", h.TextCompletion)
		r.Post("/embeddings", h.Embeddings)
		r.Post("/responses", h.Responses)

		// OpenAI dialect — the realtime WebSocket surface (llmproxy/realtime.go).
		// A WebSocket handshake is a GET, so GET is the route that matters; POST
		// is mounted beside it because the legacy pylon relay and several
		// hand-built clients post to the same path, and a 405 there is a
		// mis-diagnosable failure for a route whose real errors are already
		// hard to read.
		r.Get("/realtime", h.Realtime)
		r.Post("/realtime", h.Realtime)

		// Synthetic models surface — resolved from Postgres per project, NOT
		// routed through core (design §4.2, §3.4). The exact /models route is
		// the list; /models/* is a single-model lookup (the wildcard lets model
		// ids contain slashes, e.g. "openai/gpt-4o").
		r.Get("/models", h.Models)
		r.Get("/models/*", h.Model)

		// Connection-check surface (#319) — a real, minimal round trip to a
		// NOT-YET-SAVED credential, so it bypasses core entirely rather than
		// going through the persisted-credential Account path. See
		// llmproxy/checkconnection.go.
		r.Post("/check_connection", h.CheckConnection)

		// Model-discovery surface — the successor to legacy's
		// `import_llm_models`. It is the check's own request, to the check's
		// own hosts, through the check's own egress gate and SSRF-guarded
		// dialer; the difference is that the provider's listing is parsed and
		// its model ids are returned. See llmproxy/listprovidermodels.go.
		r.Post("/list_provider_models", h.ListProviderModels)

		// Voice-discovery surface (issue 323). Unlike the two routes above it
		// dials nothing: for every provider this gateway speaks, the voice set
		// is what the dialect publishes, not what an endpoint answers. It is
		// mounted here anyway, and behind the same signature, because provider
		// knowledge is what decides it — so a provider that DOES enumerate
		// becomes a lister in that file and no caller changes. See
		// llmproxy/listprovidervoices.go.
		r.Post("/list_provider_voices", h.ListProviderVoices)
	})

	return r
}

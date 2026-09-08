package middleware

import "net/http"

// NoStore tells a browser not to keep a copy of an API response.
//
// # The defect this closes
//
// Nothing on the JSON API said anything about caching, and "nothing" is not
// "do not cache" — it is "decide for yourself". RFC 9111 §4.2.2 lets a cache
// invent a freshness lifetime for a response that carries no explicit one, and
// WebKit does: a plain `GET` whose answer arrives with no `Cache-Control` may
// be served again from the browser's own store for the next several seconds
// without asking this service anything.
//
// Measured, on the chat rail's Context Budget panel. A journey changed the
// reader's default budget, confirmed the SERVER had resolved the new number,
// reloaded the page — and the panel went on reporting the OLD budget for the
// next twenty seconds, on WebKit only. The read it makes
// (`GET /elitea_core/context_analytics/…`) had been answered a few seconds
// earlier with the previous value and no cache directive, so the reload never
// reached this service at all. The same shape is available to every other
// read in the API: a settings page that keeps showing what an admin just
// replaced, a members list that keeps a removed member.
//
// # Why no-store rather than no-cache
//
// `no-cache` permits a stored copy and requires revalidation before it is
// used, which fixes the staleness and leaves the copy on disk. These
// responses are per-caller and authenticated — one project's credentials, one
// user's conversations — and a browser cache is shared with everything else
// running on the machine. `no-store` is the statement that fits what the body
// is, and it makes the staleness question moot rather than answered.
//
// # What it does NOT do
//
// It never replaces a value a handler chose. A route that serves something
// genuinely cacheable — the branding bootstrap's immutable answer, an
// attachment read — sets its own `Cache-Control`, and `Header().Set` in the
// handler wins over the default written here. The one rule is that the
// default is written BEFORE the handler runs, because a header set after the
// first byte never reaches the wire.
//
// It is mounted on the `/api/v2` group only. The SPA's own documents and
// hashed assets are served elsewhere and must stay cacheable.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setIfAbsent(w.Header(), "Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

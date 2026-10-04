package branding

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/httpcache"
)

// Cache-Control values per spec §4.3 channel C:
//
//   - bare URL (no ?v=): the reference index.html holds before it knows the
//     current version — must revalidate every time, hence no-cache + ETag.
//   - versioned URL (?v= matches the current ETag): content-addressed, safe
//     to cache forever.
const (
	cacheRevalidate = "no-cache"
	cacheImmutable  = "public, max-age=31536000, immutable"
)

// Config carries the handler's wiring inputs.
type Config struct {
	// PublicOrigin is the deployment's public origin
	// (publicorigin.Normalize of DEPLOYMENT_URL) that pack.json makes its
	// references absolute against. Empty means "derive it from the request".
	PublicOrigin string
	// PackPath is the file layer (BRAND_PACK_PATH env var). Empty means no
	// file layer. Ignored when Resolver is set.
	PackPath string
	// Resolver supplies the pack. Nil builds a file-only Resolver from
	// PackPath — the shape every pre-ADR-0024 caller and test used.
	Resolver *Resolver
}

// Handler serves GET /api/v2/branding/bootstrap.js.
//
// The pack is no longer fixed at construction: the Resolver merges the file
// layer with the admin-authored database layer (ADR-0024), so the body and
// its strong ETag are taken from the current Snapshot per request. The
// Resolver caches, so this costs one indexed query per TTL, not per request.
type Handler struct {
	resolver     *Resolver
	publicOrigin string
}

// NewHandler wires the handler. It never fails: every degradation path is
// the Resolver's, and lands on a lower layer with the reason logged.
func NewHandler(cfg Config) *Handler {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = NewResolver(ResolverConfig{PackPath: cfg.PackPath})
	}
	return &Handler{resolver: resolver, publicOrigin: cfg.PublicOrigin}
}

// Resolver exposes the handler's resolver so the admin save path can
// invalidate it.
func (h *Handler) Resolver() *Resolver { return h.resolver }

// noPackBody is what the endpoint serves when this deployment has no brand
// pack of its own. It deliberately leaves `window.elitea_brand` UNSET.
//
// The alternative — serving DefaultPack() — is what this endpoint used to do,
// and it was a whole-app visual regression rather than a harmless floor.
// Channel C WINS over the UI's compiled-in pack whenever the global is set
// (`shared/brand/channelC.ts`), and DefaultPack states zero scheme tokens, so
// `resolveScheme` derives all 362 ids per scheme by rotating the reference
// ramp onto `brand.hue`. With the placeholder hue (#C428DD) that rotated the
// dark scheme's cyan accent and navy surfaces ~105 degrees into magenta and
// purple-black, and swapped Montserrat for Roboto, on EVERY screen — the
// running app never looked like the product default it was supposed to fall
// back to. DefaultPack() is kept as the schema/validation reference and for
// the tests that pin its shape; it is just no longer published.
//
// A 200 with an inert body rather than a 404: `index.html` loads this as a
// blocking classic script, and an unset global is the documented
// "channel C absent" path, so this degrades exactly as intended without
// putting a network error in every user's console.
const noPackBody = "/* elitea: no deployment brand pack configured */\n"

// renderBootstrapJS serialises the pack as `window.elitea_brand = {...};`,
// or the inert body above when there is no pack to publish.
// json.Marshal HTML-escapes <, >, & (and escapes U+2028/U+2029), so the JSON
// is safe to embed in a <script> element and is a valid JS expression.
func renderBootstrapJS(p *Pack) []byte {
	if p == nil {
		return []byte(noPackBody)
	}
	data, err := json.Marshal(p)
	if err != nil {
		// Unreachable for Pack (plain data, no cycles, no custom marshalers);
		// if it ever happens, publish nothing rather than letting the
		// branding path fail — the UI then renders its own default pack.
		return []byte(noPackBody)
	}
	return append(append([]byte("window.elitea_brand = "), data...), ';')
}

// ETag returns the strong quoted entity tag of the current body (test hook
// and the value index.html templating would embed as ?v=, unquoted).
func (h *Handler) ETag() string { return h.resolver.Current(context.Background()).ETag }

// Bootstrap handles GET /api/v2/branding/bootstrap.js per spec §4.3 C.
//
// Header matrix:
//
//	bare URL                  → 200, ETag, Cache-Control: no-cache
//	?v= matches current ETag  → 200, ETag, Cache-Control: public, max-age=31536000, immutable
//	?v= mismatch              → 302 to ?v=<current>, Cache-Control: no-cache
//	If-None-Match matches     → 304, ETag + the same Cache-Control as the 200
//
// Mismatch policy: 302 redirect (not "serve current under the stale URL").
// A versioned URL is a content address served with an immutable cache
// policy; answering a stale ?v with different bytes would let any cache that
// holds that URL keep the old body forever while new clients see new bytes
// under the same address. The redirect keeps every ?v= URL's content stable
// and lands the client on the correct immutable URL in one hop. The redirect
// itself is marked no-cache so a recovered version token is never masked.
func (h *Handler) Bootstrap(w http.ResponseWriter, r *http.Request) {
	snap := h.resolver.Current(r.Context())
	v := r.URL.Query().Get("v")
	if v != "" && v != snap.ETagValue {
		w.Header().Set("Cache-Control", cacheRevalidate)
		http.Redirect(w, r, r.URL.Path+"?v="+snap.ETagValue, http.StatusFound)
		return
	}

	cacheControl := cacheRevalidate
	if v != "" { // v == h.etagValue: content-addressed URL
		cacheControl = cacheImmutable
	}
	w.Header().Set("ETag", snap.ETag)
	w.Header().Set("Cache-Control", cacheControl)

	if httpcache.ETagMatches(r.Header.Get("If-None-Match"), snap.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(snap.Body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		// HEAD carries the exact GET headers (including Content-Length of
		// the would-be body) with no body (RFC 9110 §9.3.2) — CDN and
		// monitoring probes expect this instead of a 401/405.
		return
	}
	_, _ = w.Write(snap.Body)
}

package branding

// The brand pack as JSON (ADR-0025 decision 2): GET /api/v2/branding/pack.json
// serves the SAME resolved pack as bootstrap.js — one resolver, one ETag —
// with every same-origin reference made absolute, for clients that are not a
// browser on this origin (native apps, ADR-0025).
//
// # Its own entity tag
//
// pack.json is derived from the same resolved pack STATE as bootstrap.js, but
// its BODY carries more than that state: every same-origin reference is made
// absolute against the public origin, and an unbranded deployment serves the
// embedded product default (bootstrap.js answers a constant inert body then).
// Neither the origin nor the product default is in the bootstrap ETag, and a
// matching ?v= URL is cached as immutable for a year — so pack.json's entity
// tag and ?v= token are the strong hash of ITS rendered body
// (Snapshot.PackJSONVersion), which the discovery document publishes in
// brand_pack_url. A DEPLOYMENT_URL change or a release that changes the
// product default therefore moves the token, and a client holding the old
// URL is redirected to the new one.
//
// With no configured public origin (DEPLOYMENT_URL unset) the absolute URLs
// are built from the request's own Host, so two hosts get two bodies and two
// tags. Such a response is never immutable and carries Vary.
//
// # An unbranded deployment
//
// bootstrap.js answers an inert body when no layer contributes, so the web
// app keeps its compiled-in pack. A native client has no compiled-in copy of
// THIS deployment's look, so pack.json serves the PRODUCT DEFAULT — the pack
// the web app renders in that state — with 200, never DefaultPack() (see
// noPackBody for what serving that one did). The X-Elitea-Brand-Layers
// header names the layers that contributed (`default`, `file`, `db`), so a
// client can tell "deployment default" from "branded" (coordinator
// decision 2, ADR-0025 addendum).

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/httpcache"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicorigin"
)

// PackJSONPath is the route the brand pack JSON is served at.
const PackJSONPath = "/api/v2/branding/pack.json"

// LayersHeader names the response header that lists the contributing layers.
const LayersHeader = "X-Elitea-Brand-Layers"

var (
	productDefaultOnce   sync.Once
	productDefaultShared *Pack
)

// sharedProductDefault is one parse of the embedded product default, shared
// read-only (absolutize copies before it changes anything). ProductDefault()
// re-parses on every call, which is right for callers that mutate it and
// wasteful for a per-request read path.
func sharedProductDefault() *Pack {
	productDefaultOnce.Do(func() { productDefaultShared = productDefaultPack() })
	return productDefaultShared
}

// ServedPack is the pack a client that cannot fall back to a compiled-in
// default should use: the resolved pack, or the product default when no
// layer contributes. The result is shared; callers must not modify it.
func (s Snapshot) ServedPack() *Pack {
	if s.Pack != nil {
		return s.Pack
	}
	return sharedProductDefault()
}

// DisplayName is the product name of ServedPack — the discovery document's
// display_name.
func (s Snapshot) DisplayName() string { return s.ServedPack().Product.Name }

// LayerNames lists the layers that produced ServedPack, bottom first:
// "default" when the product default is the base (no file layer), "file",
// and "db" when the admin-authored section contributed.
func (s Snapshot) LayerNames() []string {
	var names []string
	if !s.Layers.File {
		names = append(names, "default")
	} else {
		names = append(names, "file")
	}
	if s.Layers.Database {
		names = append(names, "db")
	}
	return names
}

// absolutize returns a copy of p with every same-origin reference resolved
// against origin ("scheme://host[:port]"): the five asset slots, every
// typography.fontFaces[].url and product.docsUrl. An absolute reference —
// an https:// URL or a data: URI — passes through unchanged; a root-relative
// "/path" becomes origin + "/path". The copy shares the scheme token maps
// with p; neither side modifies them.
func absolutize(p *Pack, origin string) *Pack {
	base, err := url.Parse(origin + "/")
	if err != nil || origin == "" {
		// Unreachable for an origin from publicorigin; serve the pack as is
		// rather than failing the branding path.
		slog.Warn("branding: cannot absolutise the brand pack", "origin", origin)
		return p
	}
	out := *p
	out.Assets.LogoFull = resolveRef(base, p.Assets.LogoFull)
	out.Assets.LogoMark = resolveRef(base, p.Assets.LogoMark)
	out.Assets.Favicon = resolveRef(base, p.Assets.Favicon)
	out.Assets.LoginArt = resolveOptionalRef(base, p.Assets.LoginArt)
	out.Assets.LogoEmail = resolveOptionalRef(base, p.Assets.LogoEmail)
	out.Product.DocsURL = resolveOptionalRef(base, p.Product.DocsURL)
	if p.Typography.FontFaces != nil {
		faces := make([]FontFace, len(p.Typography.FontFaces))
		for i, face := range p.Typography.FontFaces {
			face.URL = resolveRef(base, face.URL)
			faces[i] = face
		}
		out.Typography.FontFaces = faces
	}
	return &out
}

// resolveRef resolves one reference. Only a value that is not already
// absolute is touched; a data: URI and an https:// URL are absolute.
func resolveRef(base *url.URL, ref string) string {
	if ref == "" {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		slog.Warn("branding: brand pack reference does not parse; served unchanged", "ref", ref)
		return ref
	}
	if u.IsAbs() {
		return ref
	}
	return base.ResolveReference(u).String()
}

func resolveOptionalRef(base *url.URL, ref *string) *string {
	if ref == nil {
		return nil
	}
	v := resolveRef(base, *ref)
	return &v
}

// packJSONBody renders ServedPack for origin.
func packJSONBody(s Snapshot, origin string) []byte {
	data, err := json.Marshal(absolutize(s.ServedPack(), origin))
	if err != nil {
		// Unreachable for Pack (plain data); keep the endpoint answering.
		return []byte("{}")
	}
	return data
}

// packJSONEntry is one rendered pack.json: the body for one (pack state,
// origin) pair and the strong entity tag of exactly that body.
type packJSONEntry struct {
	body  []byte
	etag  string // quoted
	value string // unquoted: the ?v= token
}

// packJSONCache memoises renderings — one marshal and one hash per (pack
// state, origin), not per request. Within one process the body is a function
// of the snapshot's ETagValue (which identifies the resolved pack; the
// product default is compiled in) and the origin, so that pair is the key.
// The map is bounded: a request-derived origin (DEPLOYMENT_URL unset) comes
// from the caller's Host, so it is cleared once it holds packJSONCacheMax
// entries rather than growing.
type packJSONCache struct {
	mu      sync.Mutex
	entries map[string]*packJSONEntry
}

const packJSONCacheMax = 16

var sharedPackJSON packJSONCache

func (c *packJSONCache) get(s Snapshot, origin string) *packJSONEntry {
	key := s.ETagValue + "\n" + origin
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		return entry
	}
	if c.entries == nil || len(c.entries) >= packJSONCacheMax {
		c.entries = make(map[string]*packJSONEntry, packJSONCacheMax)
	}
	body := packJSONBody(s, origin)
	etag, value := httpcache.StrongETag(body)
	entry := &packJSONEntry{body: body, etag: etag, value: value}
	c.entries[key] = entry
	return entry
}

// PackJSONVersion is the ?v= token of pack.json as served for origin: the
// unquoted strong entity tag of its rendered body. The discovery document
// publishes it in brand_pack_url.
func (s Snapshot) PackJSONVersion(origin string) string {
	return sharedPackJSON.get(s, origin).value
}

// PackJSON handles GET and HEAD /api/v2/branding/pack.json.
//
// Header matrix — Bootstrap's, with one difference for an unconfigured
// origin:
//
//	bare URL                  → 200, ETag, Cache-Control: no-cache
//	?v= matches current ETag  → 200, ETag, immutable (no-cache when the origin is request-derived)
//	?v= mismatch              → 302 to ?v=<current>, Cache-Control: no-cache
//	If-None-Match matches     → 304, ETag + the same Cache-Control as the 200
func (h *Handler) PackJSON(w http.ResponseWriter, r *http.Request) {
	snap := h.resolver.Current(r.Context())
	origin, fromRequest := publicorigin.Resolve(h.publicOrigin, r)
	rendered := sharedPackJSON.get(snap, origin)
	v := r.URL.Query().Get("v")
	if v != "" && v != rendered.value {
		w.Header().Set("Cache-Control", cacheRevalidate)
		if fromRequest {
			w.Header().Set("Vary", publicorigin.VaryRequestOrigin)
		}
		http.Redirect(w, r, r.URL.Path+"?v="+rendered.value, http.StatusFound)
		return
	}

	cacheControl := cacheRevalidate
	if fromRequest {
		w.Header().Set("Vary", publicorigin.VaryRequestOrigin)
	} else if v != "" {
		cacheControl = cacheImmutable
	}
	w.Header().Set("ETag", rendered.etag)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set(LayersHeader, strings.Join(snap.LayerNames(), ", "))

	if httpcache.ETagMatches(r.Header.Get("If-None-Match"), rendered.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := rendered.body
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

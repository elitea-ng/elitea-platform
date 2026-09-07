package oapiserver

// Spec ↔ router conformance helpers (UI reimplementation spec §5.1, unit W1).
//
// The generated OpenAPI server in this package is not mounted by the running
// service (spec preflight fact P3), so nothing at compile time forces
// api/openapi/v2.yaml to agree with the hand-assembled chi router in
// internal/api/router.go. These helpers close that gap for tests:
//
//   - CollectRoutes walks a live chi router (build it with api.NewRouter) and
//     records every method+pattern registration, normalized so that
//     placeholder *names* do not matter ({project_id} vs {projectID}) but
//     segment *counts* always do.
//   - LoadSpecOperations parses v2.yaml with kin-openapi and returns every
//     operationId with its method, spec path and effective server base paths.
//   - RouteSet.Resolves answers "would a request shaped like this spec path
//     land on a registered route?".
//
// Reverse direction (router → spec): the spec deliberately covers only a
// subset of the router surface today (chi.Walk yields ~346 method+pattern
// registrations under the full-surface test config, ~310 after compat-shim
// exclusion), so there is no global reverse assertion. The reverse check is
// scoped to what the new UI calls: load
// apps/elitea-web/src/shared/api/endpoints.manifest.json into
// []ManifestEndpoint and assert MissingFromSpec(ops, endpoints) holds only the
// ids in testdata/reverse_check_allowlist.txt. See
// TestSpecRouterConformance/manifest_reverse_check for the wired-up seam.
//
// Do NOT point that check at apps/elitea-web/parity/manifest.json. That file is
// the parity shard INDEX. It has no `endpoints` key, so the check reads a nil
// slice and passes with zero assertions.

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
)

// Route is one method+pattern registration reported by chi.Walk.
type Route struct {
	Method  string // upper-case HTTP method
	Pattern string // full chi pattern, e.g. /api/v2/elitea_core/skills/{mode}/{projectID}

	segments []string // normalized: literals, "{}" placeholders, trailing "*"
}

// RouteSet indexes the routes of a live chi router for conformance lookups.
type RouteSet struct {
	byMethod map[string][]Route
	total    int
}

// compatShimPatterns are walked router patterns that exist only as
// compatibility or passthrough shims, NOT as spec-addressable API surface.
// They are excluded from the RouteSet because each is a wildcard registered
// for every HTTP method: leaving them in would let a drifted spec entry
// "resolve" vacuously (verified false-negative F1: with the doubled-prefix
// shim included, a bogus `POST /api/v2/zz_bogus` op matched, because root
// server `/api/v2` + path produced /api/v2/api/v2/zz_bogus).
var compatShimPatterns = map[string]string{
	// Doubled-prefix rewrite shim for admin_ui RTK Query baseUrl + explicit
	// V2_BASE (internal/api/router.go:198). It strips one /api/v2 and
	// re-dispatches into this same router, so matching it proves nothing
	// about the real target route.
	"/api/v2/api/v2/*": "doubled /api/v2 prefix rewrite shim",
	// Opaque reverse proxy to elitea-llm-gateway (internal/api/router.go:655).
	// The gateway surface is governed by its own spec and CI (ci-gateway.yml),
	// not by v2.yaml.
	"/llm/*": "reverse proxy to elitea-llm-gateway",
	// Static icon FileServer mounts (internal/api/router.go:180-181). They
	// serve files, not API operations.
	"/app/application_icon/*":      "static icon file server",
	"/app/application_tool_icon/*": "static icon file server",
}

// CollectRoutes walks r (including mounted chi sub-routers) and returns the
// full registered route table, excluding compatShimPatterns.
func CollectRoutes(r chi.Routes) (*RouteSet, error) {
	rs := &RouteSet{byMethod: map[string][]Route{}}
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if _, shim := compatShimPatterns[pattern]; shim {
			return nil
		}
		m := strings.ToUpper(method)
		rs.byMethod[m] = append(rs.byMethod[m], Route{
			Method:   m,
			Pattern:  pattern,
			segments: normalizeSegments(pattern),
		})
		rs.total++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking router: %w", err)
	}
	return rs, nil
}

// Len returns the number of method+pattern registrations collected.
func (rs *RouteSet) Len() int { return rs.total }

// Resolves reports whether a request with the given method and path shape
// would land on a registered route. The path may contain OpenAPI-style
// {placeholders}. Matching rules, per segment:
//
//   - router literal  vs path literal   → must be equal
//   - router {param}  vs anything       → matches (chi accepts any one segment,
//     so a spec literal such as "prompt_lib" is a valid value for {mode})
//   - router literal  vs path {param}   → NO match (the spec would be claiming
//     a wider surface than the router provides)
//   - router TRAILING "*"               → matches any remainder; non-trailing
//     "*" mount artifacts are collapsed at normalization (see
//     normalizeSegments), never treated as a swallow
//
// Segment counts are never normalized away: a 2-segment spec path can not
// match a 3-segment route. Trailing slashes are ignored on both sides.
// Compat/passthrough shims are excluded up front (see compatShimPatterns).
func (rs *RouteSet) Resolves(method, path string) bool {
	pathSegs := normalizeSegments(path)
	for _, rt := range rs.byMethod[strings.ToUpper(method)] {
		if segmentsMatch(rt.segments, pathSegs) {
			return true
		}
	}
	return false
}

// Patterns returns the sorted, de-duplicated "METHOD pattern" list — useful
// for debugging a conformance failure.
func (rs *RouteSet) Patterns() []string {
	seen := map[string]struct{}{}
	var out []string
	for m, routes := range rs.byMethod {
		for _, rt := range routes {
			key := m + " " + rt.Pattern
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// SpecOperation is one operationId from the OpenAPI document.
type SpecOperation struct {
	OperationID string
	Method      string   // upper-case HTTP method
	Path        string   // path as written under `paths:` in the spec
	BasePaths   []string // effective server base paths (op > pathItem > root)
}

// CandidatePaths returns the concrete path shapes a client following the spec
// could request: every effective server base joined with the spec path.
func (op SpecOperation) CandidatePaths() []string {
	if len(op.BasePaths) == 0 {
		return []string{op.Path}
	}
	out := make([]string, 0, len(op.BasePaths))
	for _, base := range op.BasePaths {
		out = append(out, base+op.Path)
	}
	return out
}

// LoadSpecOperations parses the OpenAPI document at specPath and returns
// every operation that declares an operationId, sorted by operationId.
func LoadSpecOperations(specPath string) ([]SpecOperation, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("loading OpenAPI spec %s: %w", specPath, err)
	}
	if doc.Paths == nil {
		return nil, fmt.Errorf("OpenAPI spec %s has no paths object", specPath)
	}

	rootBases, err := serverBasePaths(doc.Servers)
	if err != nil {
		return nil, err
	}

	var ops []SpecOperation
	for path, item := range doc.Paths.Map() {
		itemBases := rootBases
		if len(item.Servers) > 0 {
			if itemBases, err = serverBasePaths(item.Servers); err != nil {
				return nil, err
			}
		}
		for method, op := range item.Operations() {
			if op == nil || op.OperationID == "" {
				continue
			}
			opBases := itemBases
			if op.Servers != nil && len(*op.Servers) > 0 {
				if opBases, err = serverBasePaths(*op.Servers); err != nil {
					return nil, err
				}
			}
			ops = append(ops, SpecOperation{
				OperationID: op.OperationID,
				Method:      strings.ToUpper(method),
				Path:        path,
				BasePaths:   opBases,
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].OperationID < ops[j].OperationID })
	return ops, nil
}

// ManifestEndpoint mirrors one endpoint entry of the new UI's endpoint
// manifest (spec §5.3 `endpoints.manifest.json`, seeded from unit P1's
// apps/elitea-web/parity/manifest.json). It is the input type for the
// manifest-driven reverse check.
type ManifestEndpoint struct {
	ID          string `json:"id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operationId"`
}

// MissingFromSpec is the reverse-direction hook: it returns every manifest
// endpoint that the OpenAPI document does not cover. An endpoint is covered
// when its operationId exists in the spec, or — when the manifest entry
// carries no operationId — when its method+path matches a spec operation's
// candidate path shape under the same segment rules as RouteSet.Resolves.
func MissingFromSpec(ops []SpecOperation, endpoints []ManifestEndpoint) []ManifestEndpoint {
	byID := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		byID[op.OperationID] = struct{}{}
	}

	var missing []ManifestEndpoint
	for _, ep := range endpoints {
		if ep.OperationID != "" {
			if _, ok := byID[ep.OperationID]; ok {
				continue
			}
			missing = append(missing, ep)
			continue
		}
		if !pathCoveredBySpec(ops, ep.Method, ep.Path) {
			missing = append(missing, ep)
		}
	}
	return missing
}

// pathCoveredBySpec reports whether the document describes this method+path.
//
// THE MANIFEST PATH IS RELATIVE TO THE API ROOT, AND THIS FUNCTION USED TO
// FORGET IT. It compared the manifest path against op.CandidatePaths(), which
// is `servers[0].url` + the document path — "/api/v2/secrets/secrets/{mode}/
// {projectID}". No entry of endpoints.manifest.json carries that base (checked:
// zero of them start with "/api/"), so the first segment compared "api" against
// "secrets" and every comparison failed.
//
// Nothing looked broken, because the ONLY caller that reaches this arm is the
// hand-written half of the manifest, and a hand-written entry that fails to
// match is exactly what the reverse-check allowlist exists to hold. So the
// allowlist's own rule — "the list may only shrink; the test fails on a line
// the spec now covers" — could not fire. Measured when this was found: NINE
// allowlisted ids were already described in v2.yaml, six of them the whole
// secrets domain that issue 151 documented.
//
// The DOCUMENT path is compared instead. The base is still checked, by
// every_spec_operation_resolves_to_a_route, which is where that question
// belongs: a base that serves no operation is a defect of the DOCUMENT, not of
// one manifest entry.
//
// THE COMPARISON IS EQUALITY, NOT segmentsMatch. Both sides are TEMPLATES here,
// so a spec placeholder must face a manifest placeholder. segmentsMatch lets a
// route placeholder swallow any literal, which is right when one side is a
// concrete request path and wrong when both are templates: it read the spec's
// GET /artifacts/buckets/{projectID}/{bucket} as covering the manifest's
// GET /artifacts/buckets/default/{project_id}, where `default` is a pylon MODE
// segment and not a bucket name. Deleting that allowlist line would have
// recorded an endpoint as described that this document does not describe — the
// same class of false claim the allowlist exists to prevent.
func pathCoveredBySpec(ops []SpecOperation, method, path string) bool {
	pathSegs := normalizeSegments(path)
	m := strings.ToUpper(method)
	for _, op := range ops {
		if op.Method != m {
			continue
		}
		if sameSegments(normalizeSegments(op.Path), pathSegs) {
			return true
		}
	}
	return false
}

// sameSegments reports whether two normalized path TEMPLATES have the same
// shape. Placeholders have already collapsed to "{}", so this compares a
// literal with a literal and a placeholder with a placeholder.
func sameSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- internals ---------------------------------------------------------------

// normalizeSegments splits a chi pattern or an OpenAPI path into segments.
// Placeholder names (and chi regex constraints) collapse to "{}"; a trailing
// slash is dropped; a TRAILING chi wildcard stays "*".
//
// A "*" that is NOT the last segment is a chi.Walk mount artifact, not a
// one-segment wildcard: chi registers a mounted sub-router behind a "/*"
// node, so walking e.g. the Mount("/") at internal/api/router.go:284 with
// internal/api/gateway/budget_alerts.go:93-94 yields the pattern
// /api/v2/admin/gateway/*/budget-alerts even though the runtime request path
// is /api/v2/admin/gateway/budget-alerts — zero segments at the "*"
// position. Such segments are therefore dropped (mirroring chi.Walk's own
// "/*/" -> "/" collapse), which closes verified false-negative F2 where a
// bogus GET /admin/gateway/zz_bogus matched by swallowing "budget-alerts".
// Non-trailing "*" never appears in OpenAPI paths, so spec candidates are
// unaffected. Segment count is otherwise preserved.
func normalizeSegments(pattern string) []string {
	p := strings.TrimSuffix(pattern, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	segs := strings.Split(p, "/")
	out := segs[:0]
	for i, s := range segs {
		switch {
		case s == "*" && i < len(segs)-1:
			// mount artifact — collapse (see doc comment above)
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			out = append(out, "{}")
		default:
			out = append(out, s)
		}
	}
	return out
}

func segmentsMatch(routeSegs, pathSegs []string) bool {
	i := 0
	for ; i < len(routeSegs); i++ {
		rseg := routeSegs[i]
		if rseg == "*" {
			// Only a TRAILING wildcard may swallow a remainder.
			// normalizeSegments collapses non-trailing "*" (mount artifacts),
			// so a mid-pattern "*" is unreachable here; if one ever appears,
			// treat it strictly as exactly one segment rather than a swallow.
			if i == len(routeSegs)-1 {
				return true
			}
			if i >= len(pathSegs) {
				return false
			}
			continue
		}
		if i >= len(pathSegs) {
			return false
		}
		pseg := pathSegs[i]
		switch {
		case rseg == "{}":
			// placeholder accepts one segment of anything, literal or {}
		case pseg == "{}":
			return false // spec placeholder vs route literal: spec is wider
		case rseg != pseg:
			return false
		}
	}
	return i == len(pathSegs)
}

func serverBasePaths(servers openapi3.Servers) ([]string, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(servers))
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		u, err := url.Parse(srv.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing server URL %q: %w", srv.URL, err)
		}
		out = append(out, strings.TrimSuffix(u.Path, "/"))
	}
	return out, nil
}

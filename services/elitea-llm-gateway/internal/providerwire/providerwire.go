// Package providerwire makes bifrost/core send provider request bodies as
// compact JSON.
//
// # Why
//
// bifrost/core v1.7.x builds every converted provider request body with
// providers/utils.MarshalSortedIndent(body, "", "  ") inside
// CheckContextAndGetRequestBody, so the upstream receives two-space indented
// JSON. A web application firewall in front of an OpenAI-compatible upstream
// scores that shape differently: on the shared model endpoint, an indented
// body whose strings carry ordinary markup or comparison text (the Worker's
// `<active_instruction …>` block for a project context or an active skill,
// `a < b and c > d`, a pasted `<div>`) is answered 403 with an HTML "WAF
// block" page, while the same content sent compact passes. The Worker then
// reported every run in a project with an enabled project context as
// `model_gateway.forbidden`.
//
// The indentation carries no meaning, so this package removes it: the sorted
// encoder bifrost marshals provider bodies with is replaced, once, by one that
// keeps key sorting and answers MarshalIndent with the compact encoding. A
// compact body is also smaller on the wire.
//
// # Scope and limits
//
//   - Covers MarshalSortedIndent, the encoder of every converted provider body
//     (chat, responses, embeddings, …) for every provider class.
//   - Does NOT cover providers/utils.MergeExtraParamsIntoJSON, which re-indents
//     with encoding/json when a caller sent fields bifrost does not model and
//     extra-parameter passthrough is on. The Worker sends no such fields.
//     bifrost/core v1.8.0 sends compact bodies on both paths; upgrading to it
//     removes the need for this package.
//
// # Safety of the link
//
// A linkname binds by symbol name only. It checks neither that the target
// exists (a missing target leaves this variable nil and still builds) nor
// its type. Three guards make the seam fail closed instead:
//
//   - TestBifrostVersionIsTheVerifiedOne pins bifrost/core to
//     verifiedBifrostVersion, the release whose sortedAPI was read and found
//     to be a sonic.API. Any bump fails CI until the target is re-verified (or,
//     from v1.8.0, this package is deleted).
//   - init refuses a nil target: the gateway does not start.
//   - init then asks bifrost's own MarshalSortedIndent for a probe and refuses
//     to start unless the answer is compact, so the effect is proved on the
//     symbol bifrost really uses.
package providerwire

import (
	_ "unsafe" // for go:linkname

	"github.com/bytedance/sonic"
	// The linked variable is declared and initialised by this package; the
	// import orders its initialisation before this package's init.
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

// verifiedBifrostVersion is the bifrost/core release whose
// providers/utils.sortedAPI this package was checked against: a package-level
// sonic.API, used by MarshalSortedIndent for every converted provider body.
const verifiedBifrostVersion = "v1.7.15"

// sortedAPI is bifrost's providers/utils encoder (sonic, SortMapKeys).
//
//go:linkname sortedAPI github.com/maximhq/bifrost/core/providers/utils.sortedAPI
var sortedAPI sonic.API

// compactAPI is sortedAPI with MarshalIndent answered by Marshal.
type compactAPI struct{ sonic.API }

// MarshalIndent returns the compact, key-sorted encoding of v. The prefix and
// indent are ignored: no provider needs insignificant whitespace.
func (c compactAPI) MarshalIndent(v any, _, _ string) ([]byte, error) {
	return c.Marshal(v)
}

// init runs after providers/utils initialised sortedAPI and before any
// provider can dispatch, so no request reads a half-set value. It refuses to
// let the process start on a seam it cannot prove (see "Safety of the link").
func init() {
	if sortedAPI == nil {
		panic("providerwire: bifrost providers/utils.sortedAPI is not linked; re-verify against bifrost/core " +
			verifiedBifrostVersion)
	}
	sortedAPI = compactAPI{sortedAPI}
	probe, err := providerUtils.MarshalSortedIndent(map[string]any{"b": "<x>", "a": 1}, "", "  ")
	if err != nil || string(probe) != `{"a":1,"b":"<x>"}` {
		panic("providerwire: bifrost provider bodies are still indented; re-verify against bifrost/core " +
			verifiedBifrostVersion)
	}
}

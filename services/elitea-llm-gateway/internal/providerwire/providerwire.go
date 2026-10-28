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
// The link is by symbol name. If a bifrost upgrade renames or removes
// providers/utils.sortedAPI, the gateway stops building instead of silently
// sending indented bodies again, and TestProviderBodiesAreCompact pins the
// effect on the wire.
package providerwire

import (
	_ "unsafe" // go:linkname

	"github.com/bytedance/sonic"
	// The linked variable is declared and initialised by this package; the
	// import orders its initialisation before this package's init.
	_ "github.com/maximhq/bifrost/core/providers/utils"
)

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
// provider can dispatch, so no request reads a half-set value.
func init() {
	if _, done := sortedAPI.(compactAPI); !done {
		sortedAPI = compactAPI{sortedAPI}
	}
}

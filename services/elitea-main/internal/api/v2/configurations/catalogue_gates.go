package configurations

import (
	"fmt"
	"net/http"
)

// CatalogueGates are the two route gates of the platform LLM catalogue: the
// global providers, the platform models and the platform default model
// (ADR-0059 R0.3). router.go builds View to accept `llm.catalogue.view` or
// `configuration.governance`, and Manage to accept `llm.catalogue.manage` or
// `configuration.governance`.
//
// The catalogue routers take them as an argument rather than being mounted
// behind a group gate, so a route cannot be mounted without one.
type CatalogueGates struct {
	// View gates the reads.
	View func(http.Handler) http.Handler
	// Manage gates every write and every call that uses a stored credential
	// (a provider check, revalidate or model listing).
	Manage func(http.Handler) http.Handler
}

// must answers both gates, and refuses to mount a surface with either
// missing: a nil gate would leave the route open to every authenticated
// caller.
func (g CatalogueGates) must(surface string) (view, manage func(http.Handler) http.Handler) {
	if g.View == nil || g.Manage == nil {
		panic(fmt.Sprintf("configurations: %s mounted without its catalogue gates", surface))
	}
	return g.View, g.Manage
}

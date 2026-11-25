package adminaccess

// The administration surface at run time (ADR-0059 decision 1; story R2.7a,
// issue 2521).
//
// internal/api/adminsurface finds the surface by driving the composed router,
// in tests only: it must not be linked into the binary. A sealed session
// (browsersession/sealed.go) is honoured on the surface and nowhere else, so
// the binary needs the answer too. administration_surface.txt is that answer,
// one "METHOD pattern" line per route, patterns in RoutePattern form, and
// TestSealedSurfaceMatchesTheDrivenSurface (internal/api) keeps it equal to
// what the driver finds: a route that joins or leaves the surface fails that
// test until the file is regenerated (UPDATE_GOLDEN=1).
//
// A route the file does not list is OFF the surface. A route that is missing
// here therefore refuses the sealed session (a sign-in that reaches less),
// never the reverse.

import (
	_ "embed"
	"slices"
	"strings"
	"sync"
)

//go:embed administration_surface.txt
var administrationSurfaceFile string

var administrationSurface = sync.OnceValue(func() map[string]bool {
	routes := map[string]bool{}
	for _, line := range strings.Split(administrationSurfaceFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		routes[line] = true
	}
	return routes
})

// AdministrationSurface answers the routes of the administration surface,
// "METHOD pattern", sorted.
func AdministrationSurface() []string {
	routes := make([]string, 0, len(administrationSurface()))
	for route := range administrationSurface() {
		routes = append(routes, route)
	}
	slices.Sort(routes)
	return routes
}

// OnAdministrationSurface reports whether a request of method, routed to
// pattern (chi's pattern; RoutePattern normalises it), is on the
// administration surface. param answers the request's URL parameters: a
// route with a `{mode}` segment is on the surface in the administration mode
// only, since its twins in the other modes decide in the default mode.
func OnAdministrationSurface(method, pattern string, param func(string) string) bool {
	if pattern == "" {
		return false
	}
	pattern = RoutePattern(pattern)
	if !administrationSurface()[strings.ToUpper(method)+" "+pattern] {
		return false
	}
	if strings.Contains(pattern, "{mode}") {
		return param != nil && param("mode") == ModeAdministration
	}
	return true
}

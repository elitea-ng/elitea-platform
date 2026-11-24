package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

// administrationGatedRoutes answers the maintenance gate's question for a
// holder of a narrow administration permission (ADR-0059 R0.4b, the file doc
// of internal/api/middleware/maintenance.go): will this request reach a route
// that an administration-mode route gate decides?
//
// It keeps no list of its own. mark walks the finished router once and keeps
// every (method, pattern) whose middleware chain holds a gate of
// internal/api/middleware built for the administration mode, or the mark of
// apimw.MarkAdministrationRoute (apimw.IsAdministrationGate), and every
// endpoint that picks its gate from its {mode} segment
// (apimw.ModeGatedEndpoint). decides finds the pattern a request routes to
// with the router's own lookup. A route added with such a gate is marked with
// no edit here; a route without one, or added after mark, is not, and its
// narrow holders get the maintenance refusal: the closed failure.
type administrationGatedRoutes struct {
	routes chi.Routes
	// gated answers, for a marked route, whether the request (its route
	// context, with the path parameters) reaches the administration gate.
	gated map[string]func(*chi.Context) bool
}

func always(*chi.Context) bool { return true }

// mark records the administration-gated routes of r. It runs once, before r
// serves a request; nothing writes the set afterwards.
func (a *administrationGatedRoutes) mark(r chi.Routes) {
	gated := map[string]func(*chi.Context) bool{}
	_ = chi.Walk(r, func(method, route string, endpoint http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		key := gatedRouteKey(method, route)
		for _, mw := range middlewares {
			if apimw.IsAdministrationGate(mw) {
				gated[key] = always
				return nil
			}
		}
		if byMode, ok := endpoint.(apimw.ModeGatedEndpoint); ok {
			gated[key] = func(rctx *chi.Context) bool { return byMode.AdministrationGated(rctx.URLParam("mode")) }
		}
		return nil
	})
	a.routes, a.gated = r, gated
}

// decides reports whether req routes to a marked route. Before mark, or for a
// path no route matches, it answers false.
func (a *administrationGatedRoutes) decides(req *http.Request) bool {
	if a == nil || a.routes == nil {
		return false
	}
	path := req.URL.RawPath
	if path == "" {
		path = req.URL.Path
	}
	rctx := chi.NewRouteContext()
	pattern := a.routes.Find(rctx, req.Method, path)
	if pattern == "" {
		return false
	}
	gated, ok := a.gated[gatedRouteKey(req.Method, pattern)]
	return ok && gated(rctx)
}

// gatedRouteKey spells a route the way chi.Walk does: the wildcard of a
// sub-router mount ("/*/") is no segment of the full pattern.
func gatedRouteKey(method, pattern string) string {
	return method + " " + strings.ReplaceAll(pattern, "/*/", "/")
}

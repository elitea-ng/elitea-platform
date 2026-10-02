package browserauth

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewFormRoutes returns the complete child router for a Form-selected
// deployment. The caller mounts it at LegacyBasePath, and serves the
// CanonicalFormPaths under BasePath.
// Production composition intentionally does not mount this router yet.
func NewFormRoutes(core *CoreHandler, form *Handler) (chi.Router, error) {
	if core == nil || form == nil {
		return nil, ErrInvalidHandlerConfiguration
	}

	router := newRouter()
	core.registerRoutes(router)
	form.registerRoutes(router)
	return router, nil
}

func newRouter() chi.Router {
	router := chi.NewRouter()
	router.MethodNotAllowed(func(writer http.ResponseWriter, _ *http.Request) {
		securityHeaders(writer)
		writeProblem(writer, http.StatusBadRequest)
	})
	return router
}

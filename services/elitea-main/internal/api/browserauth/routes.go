package browserauth

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func newRouter() chi.Router {
	router := chi.NewRouter()
	router.MethodNotAllowed(func(writer http.ResponseWriter, _ *http.Request) {
		securityHeaders(writer)
		writeProblem(writer, http.StatusBadRequest)
	})
	return router
}

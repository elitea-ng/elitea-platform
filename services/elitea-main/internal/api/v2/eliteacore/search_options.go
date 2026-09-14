package eliteacore

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/entitydiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

// SearchOptions shares actor-scoped discovery with the fixed internal MCP adapter.
func (h *Handler) SearchOptions(w http.ResponseWriter, r *http.Request) {
	result, err := entitydiscovery.New(h.pool).SearchOptions(r.Context(), chi.URLParam(r, "projectID"), r.URL.Query())
	if err != nil {
		var typed *apierr.APIError
		if !errors.As(err, &typed) || typed.Status >= 500 {
			slog.ErrorContext(r.Context(), "search options read failed", "error", err)
		}
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

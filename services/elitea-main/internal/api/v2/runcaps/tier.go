package runcaps

// The tier caps read (issue 3137):
//
//	GET /api/v2/elitea_core/run_caps/prompt_lib/{projectID}/{applicationID}
//	-> {"tier": "low", "caps": {"max_tool_calls": 200, "max_model_turns": 0,
//	    "max_records_read": 0, "max_spend": 0}}
//
// The agent editor shows the platform's caps for the agent's risk tier beside
// the author's own meta.run_caps, so an author sees the ceiling before a save
// or an import refuses a cap above it (RUN_CAP_ABOVE_TIER). A cap of 0 is no
// cap. applicationID 0 is an agent not yet saved, whose tier is low: the tier
// the save check and the import apply to a new agent. The router gates the
// route on the agent read permission.

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// TierCapsPath is the route pattern, relative to /api/v2/elitea_core.
const TierCapsPath = "/run_caps/prompt_lib/{projectID}/{applicationID}"

// TierCapsReader is *workerruncaps.Checker.
type TierCapsReader interface {
	TierCaps(ctx context.Context, projectID, applicationID int64) (agentlifecycle.Tier, platformconfig.TierRunCaps, error)
}

var _ TierCapsReader = (*workerruncaps.Checker)(nil)

// TierCapsHandler serves the read. A nil handler or reader answers 503.
type TierCapsHandler struct {
	reader TierCapsReader
}

// NewTierCapsHandler builds the handler. A nil reader answers 503 (a
// composition without a database).
func NewTierCapsHandler(reader TierCapsReader) *TierCapsHandler {
	return &TierCapsHandler{reader: reader}
}

// TierCapsResponse is the body: the tier and its caps, every cap present.
type TierCapsResponse struct {
	Tier string        `json:"tier"`
	Caps TierCapValues `json:"caps"`
}

// TierCapValues are the four caps by name; 0 is no cap.
type TierCapValues struct {
	MaxToolCalls   uint32  `json:"max_tool_calls"`
	MaxModelTurns  uint32  `json:"max_model_turns"`
	MaxRecordsRead uint64  `json:"max_records_read"`
	MaxSpend       float64 `json:"max_spend"`
}

// Read answers the agent's tier and the platform's caps at it.
func (h *TierCapsHandler) Read(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.reader == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project id")
		return
	}
	applicationID, err := strconv.ParseInt(chi.URLParam(r, "applicationID"), 10, 32)
	if err != nil || applicationID < 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid application id")
		return
	}
	tier, caps, err := h.reader.TierCaps(r.Context(), projectID, applicationID)
	if err != nil {
		slog.ErrorContext(r.Context(), "run caps: read the tier caps", "project_id", projectID,
			"application_id", applicationID, "err", err)
		apierr.Write(w, apierr.Internal("the platform run caps could not be read"))
		return
	}
	writeJSON(w, http.StatusOK, TierCapsResponse{Tier: string(tier), Caps: TierCapValues{
		MaxToolCalls: caps.MaxToolCalls, MaxModelTurns: caps.MaxModelTurns,
		MaxRecordsRead: caps.MaxRecordsRead, MaxSpend: caps.MaxSpend,
	}})
}

package applications

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// CodeMemoryAutoAboveTier is the typed 422 of a version save whose
// meta.memory.write is auto for an agent above the platform's limit.
const CodeMemoryAutoAboveTier = "memory_auto_above_tier"

// MemoryAutoGate answers the two facts the save-time memory rule needs:
// the highest risk tier that may write memory with auto, and one agent's
// risk tier (ADR-0050 decision 1b, issue 1660).
type MemoryAutoGate interface {
	// MaxTier is the configured limit.
	MaxTier(ctx context.Context) (agentlifecycle.Tier, error)
	// AgentTier is the agent's registry tier. No registry row is low.
	AgentTier(ctx context.Context, projectID, applicationID int64) (agentlifecycle.Tier, error)
}

// WithMemoryAutoGate makes the version saves refuse meta.memory.write auto
// for an agent whose registry risk tier is above the platform's limit. Nil
// (a composition without the platform section) accepts auto for every tier.
func (h *Handler) WithMemoryAutoGate(gate MemoryAutoGate) *Handler {
	h.memoryAuto = gate
	return h
}

type memoryAutoRefusal struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Tier    string `json:"tier"`
	MaxTier string `json:"max_tier"`
}

// checkMemoryAuto refuses body.meta.memory.write = auto when the agent's
// risk tier is above the limit. applicationID is "" on a create (a new
// agent is low). It answers false when it wrote a response. The block's
// shape is checked before (checkMemoryBlock), so a block that does not
// parse is not this check's to refuse.
func (h *Handler) checkMemoryAuto(w http.ResponseWriter, r *http.Request, projectID, applicationID string, body map[string]any) bool {
	if h.memoryAuto == nil || body == nil {
		return true
	}
	meta, _ := body["meta"].(map[string]any)
	if meta == nil {
		return true
	}
	settings, err := agentmemory.Parse(meta[agentmemory.MetaKey])
	if err != nil || settings.Write != agentmemory.WriteAuto {
		return true
	}
	limit, err := h.memoryAuto.MaxTier(r.Context())
	if err != nil {
		return memoryAutoUnreadable(w, r, projectID, applicationID, err)
	}
	tier := agentlifecycle.TierLow
	project, _ := strconv.ParseInt(projectID, 10, 64)
	application, _ := strconv.ParseInt(applicationID, 10, 64)
	if project > 0 && application > 0 {
		if tier, err = h.memoryAuto.AgentTier(r.Context(), project, application); err != nil {
			return memoryAutoUnreadable(w, r, projectID, applicationID, err)
		}
	}
	if !tier.Above(limit) {
		return true
	}
	writeJSON(w, http.StatusUnprocessableEntity, memoryAutoRefusal{
		Error: "meta.memory.write auto is not allowed for a " + string(tier) + " tier agent (the limit is " +
			string(limit) + "): use auto_attended or review",
		Code: CodeMemoryAutoAboveTier, Tier: string(tier), MaxTier: string(limit),
	})
	return false
}

func memoryAutoUnreadable(w http.ResponseWriter, r *http.Request, projectID, applicationID string, err error) bool {
	slog.ErrorContext(r.Context(), "check the agent's memory write mode", "project_id", projectID,
		"application_id", applicationID, "err", err)
	apierr.Write(w, apierr.Internal("the agent's memory write limit could not be read"))
	return false
}

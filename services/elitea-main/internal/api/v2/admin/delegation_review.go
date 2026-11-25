package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The "Delegations awaiting review" page (ADR-0044 Amendment 1, A1.3; delegation
// epic 2156 story S6): every pending and stale delegation grant across the
// projects, counted per project, with the calling agents to open. It is the
// administrator's view of the migration of existing compositions: the grants the
// migration wrote are pending and flagged legacy, and nothing was approved.
//
// Both routes take `configuration.governance`, the permission of the Delegation
// section whose settings they explain, or the agent governance pair (ADR-0059
// R0.15): the list either name of the pair, the scan
// `agents.governance.manage`.

// DelegationReviewSource is the review queue and the scan (repos.DelegationGrants
// and repos.DelegationEdges).
type DelegationReviewSource interface {
	ReviewQueue(ctx context.Context) ([]repos.DelegationProjectQueue, error)
	ScanAll(ctx context.Context) (repos.DelegationMigrationReport, error)
}

// WithDelegationReview supplies the queue and the scan. Without it the review
// routes answer 503, and the Delegation settings carry no status.
func WithDelegationReview(source DelegationReviewSource) Option {
	return func(h *Handler) {
		if source != nil {
			h.delegationReview = source
		}
	}
}

// DelegationReviewCaller is a calling agent with grants to review.
type DelegationReviewCaller struct {
	ApplicationID int64  `json:"application_id"`
	Name          string `json:"name"`
	Pending       int    `json:"pending"`
	Stale         int    `json:"stale"`
}

// DelegationReviewProject is one project's share of the queue.
type DelegationReviewProject struct {
	ProjectID   int64                    `json:"project_id"`
	ProjectName string                   `json:"project_name"`
	Pending     int                      `json:"pending"`
	Legacy      int                      `json:"legacy"`
	Stale       int                      `json:"stale"`
	Callers     []DelegationReviewCaller `json:"callers"`
}

// DelegationReview is the page: the totals, the projects, and what the
// administrator needs to decide about the mode.
type DelegationReview struct {
	// Pending, Legacy and Stale total the projects: grants awaiting a first
	// approval, those of them the migration wrote, and grants whose callee
	// changed after approval.
	Pending int `json:"pending"`
	Legacy  int `json:"legacy"`
	Stale   int `json:"stale"`
	// Mode is the delegation mode now in force. ModeStored is false when no
	// mode was ever saved, which enforces (A1.3).
	Mode       string `json:"mode"`
	ModeStored bool   `json:"mode_stored"`
	// PrincipalBound is the principal bound switch. Report mode never relaxes it.
	PrincipalBound bool `json:"principal_bound"`
	// ReportWindowDays is the recommended length of the migration window in
	// which the mode stays report while owners review.
	ReportWindowDays int                       `json:"report_window_days"`
	Projects         []DelegationReviewProject `json:"projects"`
}

// DelegationScanResult is what a scan did.
type DelegationScanResult struct {
	Projects int `json:"projects"`
	Scanned  int `json:"scanned"`
	Failed   int `json:"failed"`
	Edges    int `json:"edges"`
	Created  int `json:"created"`
	Notified int `json:"notified"`
}

// DelegationReviewStatus is what the Delegation settings carry next to their
// values: the pending count and the report-window hint (A1.3).
func (h *Handler) delegationReviewStatus(ctx context.Context) (map[string]any, error) {
	review, err := h.readDelegationReview(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"pending":            review.Pending,
		"legacy":             review.Legacy,
		"stale":              review.Stale,
		"mode":               review.Mode,
		"mode_stored":        review.ModeStored,
		"report_window_days": review.ReportWindowDays,
	}, nil
}

func (h *Handler) readDelegationReview(ctx context.Context) (DelegationReview, error) {
	queue, err := h.delegationReview.ReviewQueue(ctx)
	if err != nil {
		return DelegationReview{}, err
	}
	settings := platformconfig.DefaultDelegationSettings()
	if h.pool != nil {
		if settings, err = platformconfig.LoadDelegationSettings(ctx, h.pool); err != nil {
			return DelegationReview{}, err
		}
	}
	review := DelegationReview{
		Mode: string(settings.Mode), ModeStored: settings.Stored, PrincipalBound: settings.PrincipalBound,
		ReportWindowDays: platformconfig.DelegationReportWindowDays, Projects: []DelegationReviewProject{},
	}
	for _, project := range queue {
		out := DelegationReviewProject{
			ProjectID: project.ProjectID, ProjectName: project.ProjectName,
			Pending: project.Pending, Legacy: project.Legacy, Stale: project.Stale, Callers: []DelegationReviewCaller{},
		}
		for _, caller := range project.Callers {
			out.Callers = append(out.Callers, DelegationReviewCaller{
				ApplicationID: caller.ApplicationID, Name: caller.Name, Pending: caller.Pending, Stale: caller.Stale,
			})
		}
		review.Pending += project.Pending
		review.Legacy += project.Legacy
		review.Stale += project.Stale
		review.Projects = append(review.Projects, out)
	}
	return review, nil
}

// DelegationReviewList serves `GET /admin/delegation_review`.
func (h *Handler) DelegationReviewList(w http.ResponseWriter, r *http.Request) {
	if h.delegationReview == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the delegation review is not available"})
		return
	}
	review, err := h.readDelegationReview(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "delegation review: the queue was not read", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the delegation review queue"})
		return
	}
	writeJSON(w, http.StatusOK, review)
}

// DelegationReviewScan serves `POST /admin/delegation_review/scan`: the migration
// again, for every project, writing only the pending grants that are missing. It
// approves nothing.
func (h *Handler) DelegationReviewScan(w http.ResponseWriter, r *http.Request) {
	if h.delegationReview == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the delegation review is not available"})
		return
	}
	report, err := h.delegationReview.ScanAll(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "delegation review: the scan did not finish", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the scan did not finish"})
		return
	}
	writeJSON(w, http.StatusOK, DelegationScanResult{
		Projects: report.Projects, Scanned: report.Migrated, Failed: report.Failed,
		Edges: report.Edges, Created: report.Created, Notified: report.Notified,
	})
}

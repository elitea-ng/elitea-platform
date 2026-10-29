// Package workeregress serves the admin view of the public hosts runs reached
// and were refused (ADR-0044 decision 2, issue #1490):
//
//	GET /api/v2/admin/worker_egress/hosts?days=30&source=worker&project_id=
//
// It aggregates the stored egress summaries per project, agent and host, so
// an operator can seed the platform's or a project's worker egress allowlist
// from what the agents actually used before switching `worker_egress` to
// `allowlist`. It needs `configuration.governance` (administration mode), the
// permission of the worker egress settings themselves.
package workeregress

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Permission is the route's grant: the worker egress settings' own.
const Permission = "configuration.governance"

// HostsPath is the route under /api/v2.
const HostsPath = "/admin/worker_egress/hosts"

// Store answers a hosts query (repos.EgressRunSummaries).
type Store interface {
	Hosts(ctx context.Context, query app.HostsQuery) (app.HostsReport, error)
}

// Handler serves the route. A zero Handler answers 503.
type Handler struct {
	store Store
	now   func() time.Time
}

// NewHandler builds the handler.
func NewHandler(store Store) *Handler { return &Handler{store: store, now: time.Now} }

type reachedView struct {
	ProjectID   int64  `json:"project_id"`
	AgentID     *int64 `json:"agent_id"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Connections int64  `json:"connections"`
	Executions  int64  `json:"executions"`
	LastSeen    string `json:"last_seen"`
}

type refusedView struct {
	ProjectID  int64  `json:"project_id"`
	AgentID    *int64 `json:"agent_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Reason     string `json:"reason"`
	Count      int64  `json:"count"`
	Executions int64  `json:"executions"`
	LastSeen   string `json:"last_seen"`
}

type hostsView struct {
	Days             int           `json:"days"`
	Since            string        `json:"since"`
	Source           string        `json:"source"`
	ProjectID        *int64        `json:"project_id"`
	Reached          []reachedView `json:"reached"`
	Refused          []refusedView `json:"refused"`
	ReachedTruncated bool          `json:"reached_truncated"`
	RefusedTruncated bool          `json:"refused_truncated"`
}

// Hosts answers the hosts reached and refused in the last `days` days
// (default 30, at most 90), from `source` (worker, the default; proxy; or
// all), optionally for one project.
func (h *Handler) Hosts(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, apierr.Response{Error: "the egress summary store is not configured"})
		return
	}
	query := r.URL.Query()
	days := app.DefaultHostsDays
	if raw := query.Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > app.MaxHostsDays {
			apierr.Write(w, apierr.BadRequest("days must be a whole number from 1 to 90"))
			return
		}
		days = parsed
	}
	source := app.SourceWorker
	if raw := query.Get("source"); raw != "" {
		switch app.Source(raw) {
		case app.SourceWorker, app.SourceProxy, app.SourceAll:
			source = app.Source(raw)
		default:
			apierr.Write(w, apierr.BadRequest("source must be worker, proxy or all"))
			return
		}
	}
	var projectID *int64
	if raw := query.Get("project_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			apierr.Write(w, apierr.BadRequest("project_id must be a positive integer"))
			return
		}
		projectID = &parsed
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	since := now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	request := app.HostsQuery{Since: since, Source: source, Limit: app.MaxHostsRows}
	if projectID != nil {
		request.ProjectID = *projectID
	}
	report, err := h.store.Hosts(r.Context(), request)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	view := hostsView{
		Days: days, Since: since.Format(time.RFC3339), Source: string(source), ProjectID: projectID,
		Reached: make([]reachedView, 0, len(report.Reached)), Refused: make([]refusedView, 0, len(report.Refused)),
		ReachedTruncated: report.ReachedTruncated, RefusedTruncated: report.RefusedTruncated,
	}
	for _, host := range report.Reached {
		view.Reached = append(view.Reached, reachedView{
			ProjectID: host.ProjectID, AgentID: host.AgentID, Host: host.Host, Port: host.Port,
			Connections: host.Connections, Executions: host.Executions, LastSeen: host.LastSeen.UTC().Format(time.RFC3339),
		})
	}
	for _, host := range report.Refused {
		view.Refused = append(view.Refused, refusedView{
			ProjectID: host.ProjectID, AgentID: host.AgentID, Host: host.Host, Port: host.Port, Reason: host.Reason,
			Count: host.Count, Executions: host.Executions, LastSeen: host.LastSeen.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, view)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

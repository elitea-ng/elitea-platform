package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

type Repository interface {
	GetUsageSummary(ctx context.Context, params analytics.QueryParams) (analytics.UsageSummary, error)
	GetAgentAnalytics(ctx context.Context, params analytics.QueryParams) (analytics.AgentBreakdown, error)
	GetToolAnalytics(ctx context.Context, params analytics.QueryParams) ([]analytics.ToolAnalytics, error)
	// GetUserActivity also reports whether it had to cut the list. See the
	// repository's cap constants for why a silent cut is worse here than
	// elsewhere: the client paginates over what it receives.
	GetUserActivity(ctx context.Context, params analytics.QueryParams) ([]analytics.UserActivity, bool, error)
}

type Handler struct {
	repo Repository
	// now is the clock the default date window ends at. Tests pin it;
	// production leaves it nil.
	now func() time.Time
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

// WithClock pins the clock the default date window is measured back from.
func (h *Handler) WithClock(now func() time.Time) *Handler {
	h.now = now
	return h
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.Usage)
	r.Get("/agents", h.Agents)
	r.Get("/tools", h.Tools)
	r.Get("/users", h.Users)
	return r
}

// writeRepoFailure is the one exit these routes take when the repository could
// not answer.
//
// It replaces `summary, _ := h.repo.Get...` followed by 200-and-zeros
// (issue #303). Discarding the error made every failure mode — a table that has
// never existed, a dropped connection, a permission error — arrive at the
// browser as "this project used nothing", which is a claim, not a blank.
//
// The repository's reason travels in the body rather than only to the log,
// because the operator who sees this endpoint is looking at a browser.
//
// # 400 FOR A BAD REQUEST, 501 FOR AN ABSENT SOURCE, 500 FOR A FAILED QUERY
//
// The two are not the same event and must not carry the same status. A failed
// query is worth retrying; an absent producer is the server's final answer, and
// it will be the final answer to the next identical request too.
//
// Collapsing them into 500 had a measured cost. Every client that treats 5xx as
// transient retries: the web app's TanStack Query default retries once on any
// 5xx (apps/elitea-web/src/app/providers/queryClient.ts), so a HAR of one
// Analytics page load against elitea.technicaldomain.xyz holds each of these
// four endpoints TWICE, and every one of the eight requests was answered by a
// branch that had already decided it had nothing to answer with. 501 Not
// Implemented is the exact semantics — the server does not support the
// functionality required to fulfil the request — and it is one a client can
// classify as final without parsing a message string.
func writeRepoFailure(w http.ResponseWriter, err error) {
	// The caller's mistake, not the server's. A 500 here would blame the
	// service for a path segment it was handed, and would be retried.
	if errors.Is(err, analytics.ErrBadProject) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "project id must be a positive integer",
			"code":  "bad_project_id",
		})
		return
	}
	if errors.Is(err, analytics.ErrNoSource) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "analytics is not available on this deployment",
			// Machine-readable, so a client branches on this rather than on the
			// prose above. The prose is for the operator reading the network
			// tab; the code is for the screen deciding what to render.
			"code":   "no_data_source",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": "failed to query analytics",
		"code":  "query_failed",
	})
}

// Usage is the Overview tab.
//
// # Every key here is a figure the repository measured
//
// The six KPI figures that used to be written as literal 0 — unique_users,
// total_project_users, ai_active_users, adoption_rate, tool_runs, chat_msgs —
// were removed rather than zeroed, because nothing computed them: the response
// was asserting six counts it had never looked up. Three of them now have a
// producer (gateway.llm_request_logs, shared migration 0099) and are back as
// REAL numbers; three still do not, and stay absent.
//
//	ai_active_users     distinct user_id on the request log — every identity
//	                    that called a model here, membership or not.
//	total_project_users distinct member of the project. ABSENT when the
//	                    identity tables are (see the repository) — an adoption
//	                    rate over an invented denominator is worse than none.
//	active_project_members
//	                    the INTERSECTION of the two: members who called. Absent
//	                    with the denominator, because it is measured in the same
//	                    statement.
//	adoption_rate       active_project_members over total_project_users, as a
//	                    percentage. NOT ai_active_users over total_project_users
//	                    — those two count different populations, and dividing
//	                    one by the other reported "300% adoption" on a project
//	                    with one member and three non-member callers.
//	llm_calls           count of request-log rows. A real per-call count, not
//	                    the accumulator's count of billing PERIODS.
//	total_tokens        prompt + completion, summed.
//
// The three figures below have no producer. Each probe names the assignment
// that closing the gap would create, so the day one of them is answerable this
// comment fails the build instead of going quietly stale (issue 621).
//
// DISCLOSURE-CHECK: absent `kpis["tool_runs"]` in services/elitea-main/internal/api/v2/analytics/handler.go
// DISCLOSURE-CHECK: absent `kpis["chat_msgs"]` in services/elitea-main/internal/api/v2/analytics/handler.go
// DISCLOSURE-CHECK: absent `kpis["agent_runs"]` in services/elitea-main/internal/api/v2/analytics/handler.go
//
//	tool_runs           ABSENT — no producer.
//	chat_msgs           ABSENT — no producer.
//	agent_runs          ABSENT — no producer. It used to be set to the same
//	                    value as llm_calls, which is not a smaller claim than
//	                    zero: it asserted that every LLM call is an agent run.
//	total_cost          ABSENT — it HAS a producer, and /analytics_costs is it.
//	                    (The `health` block beside `kpis` carries the failure
//	                    and latency view; see the repository's projectHealth.)
//	                    Reading the same accumulator a second way here would be
//	                    a second view of the same money that could disagree
//	                    with the first, and only one of the two carries the
//	                    scope rules that stop it double-counting.
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	summary, err := h.repo.GetUsageSummary(r.Context(), h.parseParams(r))
	if err != nil {
		writeRepoFailure(w, err)
		return
	}

	kpis := map[string]any{
		"llm_calls":       summary.TotalRuns,
		"total_tokens":    summary.TotalTokens,
		"ai_active_users": summary.ActiveUsers,
	}
	// `total > 0` gates the PUBLICATION of the pair, not only the rate.
	//
	// The tile prints the caller count over this denominator: "AI ACTIVE 1 of 4
	// members". A denominator of 0 beside a numerator of 1 is not a fact about
	// the project — one caller cannot be one of none — it says the membership
	// source did not describe this project. That state rendered as
	// "AI ACTIVE 1 of 0 members" on every fresh install. Omitting the pair makes
	// the tile drop its suffix and report the caller count alone, which is the
	// figure the request log can defend.
	if summary.TotalProjectUsers != nil && summary.ActiveMembers != nil && *summary.TotalProjectUsers > 0 {
		total := *summary.TotalProjectUsers
		activeMembers := *summary.ActiveMembers
		kpis["total_project_users"] = total
		kpis["active_project_members"] = activeMembers
		// The numerator is ACTIVE MEMBERS, not active callers. Callers include
		// identities the membership table does not contain — a removed member, a
		// global administrator, a service token — so dividing by the member
		// count produced rates above 100% routinely (measured: 3 callers, 1
		// member, "300% adoption"). See projectAdoption in the repository.
		kpis["adoption_rate"] = round1(float64(activeMembers) / float64(total) * 100)
	}

	body := map[string]any{
		"kpis":           kpis,
		"top_ai_users":   nonNil(summary.TopUsers),
		"daily_activity": nonNil(summary.DailyActivity),
		"models":         nonNil(summary.ByModel),
		// Stated rather than implied, like the users list's `truncated`. The
		// client sums `models` to normalise its share column, so a cut it
		// cannot see turns every share into a percentage of the busiest N.
		"models_truncated": summary.ModelsTruncated,
	}
	// The Health tab renders from this same response — AnalyticsContainer
	// fetches /analytics for tab 0 and tab 4 alike — so the block travels with
	// it rather than on an endpoint of its own.
	//
	// Absent rather than empty when the repository could not build it, for the
	// reason every other figure here is: a Health object with zero totals is
	// the true report of a project with no traffic, and using the same shape to
	// mean "we could not look" would make the two indistinguishable.
	if summary.Health != nil {
		body["health"] = summary.Health
	}
	writeJSON(w, http.StatusOK, body)
}

// Agents is the Agents tab: which agents ran in the window and how they behaved.
//
// # `items` is ABSENT, not empty, when the dimension is unavailable
//
// gateway.llm_request_logs gained its execution_id column in shared migration
// 0100, and nothing on a row written before it identifies an agent — the log
// records the model a request addressed, never what composed it. A backfill is
// impossible rather than merely unwritten, so a window that predates the
// migration has no agent data at all.
//
// Emitting `items: []` for that window would say "no agent ran", which for a
// month in which agents ran constantly is a false measurement served with a 200
// — the fallback-body failure this codebase keeps meeting. So the flag decides
// the SHAPE: with agent_dimension_available false there is no items key for a
// client to map over, and the tab has to render the absence.
//
// The precedent is usageDimensions.Available
// (internal/api/v2/budgets/usage_dimensions.go), which omits its dimensional
// block for a deployment upgraded mid-period rather than zero-filling it.
//
// attributed_llm_calls / unattributed_llm_calls travel either way, because the
// per-agent rows are NOT a partition of the overview's llm_calls tile: most
// /llm traffic is not made from a runtime execution, so without both figures an
// operator is reconciling a breakdown against a total it never summed to.
func (h *Handler) Agents(w http.ResponseWriter, r *http.Request) {
	breakdown, err := h.repo.GetAgentAnalytics(r.Context(), h.parseParams(r))
	if err != nil {
		writeRepoFailure(w, err)
		return
	}

	body := map[string]any{
		"agent_dimension_available": breakdown.Available,
		"attributed_llm_calls":      breakdown.AttributedCalls,
		"unattributed_llm_calls":    breakdown.UnattributedCalls,
	}
	if breakdown.Available {
		body["items"] = nonNil(breakdown.Agents)
		body["truncated"] = breakdown.Truncated
	}

	// The DETAIL view is still a stub, and it is now a stub for ONE reason
	// rather than two. Its shape is { entity_name, users, tools, daily_usage },
	// and `tools` is the dimension this platform still has no producer for —
	// see GetToolAnalytics. Rather than answer with four empty lists, which
	// would report an agent that did nothing, the detail branch reports the
	// same availability flag: the agent half is answerable and the tool half is
	// not, and the two must not render alike.
	if r.URL.Query().Get("application_id") != "" || r.URL.Query().Get("agent_id") != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_dimension_available": breakdown.Available,
			"tool_dimension_available":  false,
			"entity_name":               agentName(breakdown, r),
			"daily_usage":               []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// agentName resolves the requested agent's display name out of the breakdown
// the list read already produced, so the detail header does not need a second
// query. Empty when the agent made no request in the window, which is a true
// statement about it rather than a missing lookup.
func agentName(breakdown analytics.AgentBreakdown, r *http.Request) string {
	wanted := r.URL.Query().Get("application_id")
	if wanted == "" {
		wanted = r.URL.Query().Get("agent_id")
	}
	for _, agent := range breakdown.Agents {
		if agent.ApplicationID == wanted {
			return agent.Name
		}
	}
	return ""
}

func (h *Handler) Tools(w http.ResponseWriter, r *http.Request) {
	tools, err := h.repo.GetToolAnalytics(r.Context(), h.parseParams(r))
	if err != nil {
		writeRepoFailure(w, err)
		return
	}

	// Detail view expects { entity_name, users, agents, daily_usage }
	if r.URL.Query().Get("tool_id") != "" || r.URL.Query().Get("toolkit_id") != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"entity_name": "",
			"users":       []any{},
			"agents":      []any{},
			"daily_usage": []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(tools)})
}

// Users is the Users tab: every member who called a model in the window.
//
// The DETAIL branch is still a stub. It is reachable — the web client sends
// `user_id` when a row is clicked — and what it would need is a per-user split
// by agent and by tool, which is exactly the two dimensions the request log
// does not carry. It answers with empty lists and no kpis block rather than
// with zeros, for the same reason the list branches refuse outright.
func (h *Handler) Users(w http.ResponseWriter, r *http.Request) {
	users, truncated, err := h.repo.GetUserActivity(r.Context(), h.parseParams(r))
	if err != nil {
		writeRepoFailure(w, err)
		return
	}

	// Detail view expects { entity_name, agents, tools, daily_usage }
	if r.URL.Query().Get("user_id") != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"entity_name": "",
			"agents":      []any{},
			"tools":       []any{},
			"daily_usage": []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": nonNil(users),
		// Stated rather than implied, the way /analytics_costs states
		// `periods_truncated`: with this false, `items` is every caller in the
		// window; with it true, it is the busiest N of them. The client
		// paginates and searches client-side over this array and would
		// otherwise present a cut list as the whole membership.
		"truncated": truncated,
	})
}

// nonNil renders an empty result as `[]` rather than `null`. A JSON null and an
// empty array are different things to a client that maps over the value, and
// only one of them is what "no rows" means.
func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

// round1 keeps the adoption rate to one decimal. The UI prints it directly, and
// 47.36842105263158% is not a more accurate statement about 9 of 19 people.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// parseParams resolves the request's scope ONCE, including the date window.
//
// The window is resolved here rather than in each repository method because
// /analytics_costs already had to make the same defaulting and clamping
// decisions, and two endpoints on the same screen answering over two different
// windows is a discrepancy no reader can attribute. dateWindow is shared with
// it for exactly that reason.
func (h *Handler) parseParams(r *http.Request) analytics.QueryParams {
	query := r.URL.Query()
	startDate := query.Get("start_date")
	if startDate == "" {
		startDate = query.Get("date_from")
	}
	endDate := query.Get("end_date")
	if endDate == "" {
		endDate = query.Get("date_to")
	}
	// NORMALISED before dateWindow, not passed through raw. These routes have
	// always accepted `start_date`/`end_date` as well as the `date_from`/
	// `date_to` the web client sends, and dateWindow — which is
	// /analytics_costs' transcription of the pylon reference — reads only the
	// second pair. Handing it the raw query would silently give a caller using
	// the first pair the DEFAULT window instead of the one they asked for: a
	// plausible wrong answer rather than an error, over a range nobody
	// requested.
	window := url.Values{}
	if startDate != "" {
		window.Set("date_from", startDate)
	}
	if endDate != "" {
		window.Set("date_to", endDate)
	}
	from, to := dateWindow(window, h.clock)
	return analytics.QueryParams{
		ProjectID: chi.URLParam(r, "projectID"),
		From:      from,
		To:        to,
		StartDate: startDate,
		EndDate:   endDate,
		Period:    query.Get("period"),
	}
}

func (h *Handler) clock() time.Time {
	if h.now == nil {
		return time.Now().UTC()
	}
	return h.now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

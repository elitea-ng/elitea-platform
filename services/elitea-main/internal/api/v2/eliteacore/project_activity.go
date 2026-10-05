package eliteacore

// `GET /elitea_core/project_user_activity/{mode}` — per-user event counts for
// ONE project over an optional time window (unit A14, issue #200).
//
// The admin Projects page's activity drawer draws one square per project
// member, coloured by whether that member did anything in the selected range.
// Without this endpoint the drawer has member names and no activity, so every
// square renders inactive — a chart that is a constant, which is the defect
// class this unit exists to remove.
//
// ## What was here before
//
// Nothing. elitea-main had no route and no handler; the reference page's
// `useProjectUserActivityQuery` hit a 404 and the component's only error path
// renders "No users found".
//
// ## The contract is not invented
//
// It mirrors legacy/plugins/elitea_core/api/v2/project_user_activity.py — same
// path, same `project_id` / `date_from` / `date_to` parameters, same
// `{rows:[{user_id,user_email,event_count}]}` body — and reads the same
// `centry.audit_events` table the four audit endpoints in ./audit.go read.
//
// ## One deliberate divergence
//
// pylon groups by `(user_id, user_email)`. `user_email` is denormalised onto
// every audit row at write time, so a user whose address changed — or whose
// events were written by two code paths that spell it differently — occupies
// TWO rows there, both keyed to the same `user_id`. The client indexes its
// activity map by `user_id` (`activityMap.set(row.user_id, row.event_count)`),
// so the second row silently overwrites the first and the square shows a
// PARTIAL count. This groups by `user_id` alone and reports the address from
// the most recent event, so each user appears exactly once with their whole
// count. It is the same row-multiplication shape as the admin user listing's
// LEFT JOIN, one aggregation level up.
//
// ## Authorisation
//
// Gated in internal/api/router.go on the permission the pylon original declares
// (`models.admin.audit_trail.view`), resolved from auth_core__user_role per
// request — the same gate the audit reads carry, because this is the same data
// counted differently. No event content is written to a log line.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// projectUserActivityRow is one member's event count. `user_email` is nullable
// because `centry.audit_events.user_email` is: an event may name a user id
// whose address was never recorded.
type projectUserActivityRow struct {
	UserID     int64   `json:"user_id"`
	UserEmail  *string `json:"user_email"`
	EventCount int64   `json:"event_count"`
}

// ProjectUserActivity serves `GET /elitea_core/project_user_activity/{mode}`.
func (h *Handler) ProjectUserActivity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	raw := query.Get("project_id")
	if raw == "" {
		apierr.WriteStatus(w, http.StatusBadRequest, "project_id is required")
		return
	}
	projectID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || projectID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "project_id must be a positive integer")
		return
	}

	dateFrom, dateTo := optionalTime(query.Get("date_from")), optionalTime(query.Get("date_to"))
	if reversedDateRange(dateFrom, dateTo) {
		writeInvalidDateRange(w)
		return
	}
	rows, err := h.listProjectUserActivity(r.Context(), projectID, dateFrom, dateTo)
	if err != nil {
		// Reported as a failure rather than degraded to an empty list: "nobody
		// was active in this window" and "the query blew up" render identically
		// in the drawer, and choosing the reassuring one is how the admin user
		// listing used to hide its own errors.
		auditReadFailed(w, "project user activity")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (h *Handler) listProjectUserActivity(
	ctx context.Context, projectID int64, dateFrom, dateTo *time.Time,
) ([]projectUserActivityRow, error) {
	if h.pool == nil {
		return []projectUserActivityRow{}, nil
	}

	args := &argList{}
	where := fmt.Sprintf("e.project_id = %s AND e.user_id IS NOT NULL", args.add(projectID))
	if dateFrom != nil {
		where += fmt.Sprintf(" AND e.timestamp >= %s", args.add(*dateFrom))
	}
	if dateTo != nil {
		where += fmt.Sprintf(" AND e.timestamp <= %s", args.add(*dateTo))
	}

	// `user_email` is picked by DISTINCT ON's ordering rather than grouped on —
	// see this file's header. `ORDER BY count DESC` alone is not a total order,
	// so `user_id` is the tiebreaker: without it, two members with equal counts
	// swap places between refreshes of an unchanged table.
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
SELECT counted.user_id, counted.user_email, counted.event_count
FROM (
    SELECT DISTINCT ON (e.user_id)
           e.user_id,
           e.user_email,
           COUNT(*) OVER (PARTITION BY e.user_id) AS event_count
    FROM %s AS e
    WHERE %s
    ORDER BY e.user_id, e.timestamp DESC NULLS LAST, e.id DESC
) AS counted
ORDER BY counted.event_count DESC, counted.user_id`, auditTable, where), args.values...)
	if err != nil {
		return nil, fmt.Errorf("query project user activity: %w", err)
	}
	defer rows.Close()

	items := make([]projectUserActivityRow, 0)
	for rows.Next() {
		var row projectUserActivityRow
		if err := rows.Scan(&row.UserID, &row.UserEmail, &row.EventCount); err != nil {
			return nil, fmt.Errorf("scan project user activity row: %w", err)
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project user activity rows: %w", err)
	}
	return items, nil
}

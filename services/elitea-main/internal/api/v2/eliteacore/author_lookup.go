package eliteacore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// authorCountedProjectsMax bounds how many projects Author counts an author's
// entities over. Each counted project costs two statements, so this is the
// bound on the work one request can cause. An author who belongs to more
// projects than this is counted over the lowest-numbered ones.
const authorCountedProjectsMax = 100

// Author answers the author card: the person's name, avatar and description,
// plus the number of agents, pipelines, toolkits and collections they authored.
//
// What a caller may learn depends on how they relate to the author:
//
//   - the author themself, or a user who shares at least one project with
//     them: the e-mail is included, and the counts are taken over the projects
//     the two have in common (for the author themself, all their projects);
//   - anyone else: no `email` key at all (not an empty string) and zero counts.
//     The public project never makes two users colleagues: sign-up enrolment
//     can put every user in it. The author's own card still counts it.
//
// An id that names no user, an id that is not a number and an id the caller has
// no relation to all answer 200; the first two answer the same empty object,
// and the third differs only by the absent e-mail, so the status code does not
// reveal whether a user id exists.
func (h *Handler) Author(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	caller, ok := user.OwningUserID()
	if !ok || caller > math.MaxInt32 {
		apierr.WriteStatus(w, http.StatusForbidden, "forbidden")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "author lookup unavailable")
		return
	}
	author, err := strconv.ParseInt(chi.URLParam(r, "authorID"), 10, 32)
	if err != nil || author <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	var name, email, avatar, desc string
	err = h.pool.QueryRow(ctx, `
		SELECT COALESCE(au.name, ''), COALESCE(au.email, ''), COALESCE(su.avatar, ''), COALESCE(su.description, '')
		FROM auth_core__user au
		LEFT JOIN centry.social_users su ON su.user_id = au.id
		WHERE au.id = $1
	`, int32(author)).Scan(&name, &email, &avatar, &desc)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "author lookup: profile read failed", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "author lookup failed")
		return
	}

	// Zero excludes nothing: the author's own card covers all their projects.
	var excluded int32
	if int32(caller) != int32(author) {
		if public, err := strconv.ParseInt(publicProjectIDOrDefault(), 10, 32); err == nil {
			excluded = int32(public)
		}
	}
	shared, err := h.sharedProjects(ctx, int32(caller), int32(author), excluded)
	if err != nil {
		slog.ErrorContext(ctx, "author lookup: shared project read failed", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "author lookup failed")
		return
	}

	var totalApps, totalPipelines, totalToolkits, totalCollections int
	for _, project := range shared {
		counts, err := h.authorCounts(ctx, project, int32(author))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.ErrorContext(ctx, "author lookup: count failed", "project_id", project, "error", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "author lookup failed")
			return
		}
		totalApps += counts.apps
		totalPipelines += counts.pipelines
		totalToolkits += counts.toolkits
		totalCollections += counts.collections
	}

	body := map[string]any{
		"id": int(author), "name": name,
		"avatar": avatar, "title": "", "description": desc,
		"total_conversations": 0, "public_conversations": 0,
		"public_applications": 0, "total_applications": totalApps,
		"public_pipelines": 0, "total_pipelines": totalPipelines,
		"total_toolkits": totalToolkits, "public_collections": 0,
		"total_collections": totalCollections, "rewards": 0,
	}
	// The e-mail is for the author and for people who work with them.
	if int32(caller) == int32(author) || len(shared) > 0 {
		body["email"] = email
	}
	writeJSON(w, http.StatusOK, body)
}

// sharedProjects returns the ids of the projects both users hold a role in,
// except excluded, in ascending order and at most authorCountedProjectsMax of
// them. Called with the same user twice it returns that user's own projects.
func (h *Handler) sharedProjects(ctx context.Context, caller, author, excluded int32) ([]int32, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT DISTINCT author_role.project_id
		FROM auth_core__project_user_role author_role
		JOIN auth_core__project_user_role caller_role
		  ON caller_role.project_id = author_role.project_id AND caller_role.user_id = $1
		WHERE author_role.user_id = $2 AND author_role.project_id <> $4
		ORDER BY author_role.project_id
		LIMIT $3`, caller, author, authorCountedProjectsMax, excluded)
	if err != nil {
		return nil, fmt.Errorf("query shared projects: %w", err)
	}
	defer rows.Close()
	projects := make([]int32, 0, 8)
	for rows.Next() {
		var project int32
		if err := rows.Scan(&project); err != nil {
			return nil, fmt.Errorf("scan shared project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read shared projects: %w", err)
	}
	return projects, nil
}

type authorEntityCounts struct{ apps, pipelines, toolkits, collections int }

// authorCounts counts what the author authored in one project's tenant schema.
//
// The author of an agent is the author of its VERSIONS.
// `applications.owner_id` is the owning PROJECT (#533), so counting on it
// counted the agents of the project whose id happens to equal the user id — a
// different set, and usually an empty one.
//
// A schema or table that does not exist contributes zero (a project that is
// still provisioning, or a deployment without collections). Any other failure
// is returned: a count that is silently zero because the database refused is
// not a count.
func (h *Handler) authorCounts(ctx context.Context, project, author int32) (authorEntityCounts, error) {
	var counts authorEntityCounts
	s := catalogueSchema("p_" + strconv.Itoa(int(project)))
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT
		 (SELECT COUNT(*) FROM %[1]s.applications a
		   WHERE EXISTS (SELECT 1 FROM %[1]s.application_versions av WHERE av.application_id = a.id AND av.author_id = $1)
		     AND NOT EXISTS (SELECT 1 FROM %[1]s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline')),
		 (SELECT COUNT(*) FROM %[1]s.applications a
		   WHERE EXISTS (SELECT 1 FROM %[1]s.application_versions av WHERE av.application_id = a.id AND av.author_id = $1)
		     AND EXISTS (SELECT 1 FROM %[1]s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline')),
		 (SELECT COUNT(*) FROM %[1]s.elitea_tools WHERE author_id = $1)`, s), author).
		Scan(&counts.apps, &counts.pipelines, &counts.toolkits)
	if err != nil && !isMissingRelation(err) {
		return authorEntityCounts{}, fmt.Errorf("count authored entities: %w", err)
	}
	err = h.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s.prompt_collections WHERE author_id = $1`, s), author).
		Scan(&counts.collections)
	if err != nil && !isMissingRelation(err) {
		return authorEntityCounts{}, fmt.Errorf("count authored collections: %w", err)
	}
	return counts, nil
}

// isMissingRelation reports SQLSTATE 3F000 (no such schema) and 42P01 (no such
// table).
func isMissingRelation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "3F000" || pgErr.Code == "42P01")
}

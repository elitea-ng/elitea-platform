package eliteacore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The public catalogue list — `GET /elitea_core/public_applications/prompt_lib`.
//
// The pylon route this replaces (legacy/plugins/elitea_core/api/v2/
// public_applications.py, which calls utils/application_utils.py's
// list_applications_api) accepts a search string, a status list, an agent-type
// filter, a sort key with a direction, and limit/offset pagination, and answers
// {total, rows} where each row carries its tags and its like counters. The Go
// port read only ?category and answered a hard `ORDER BY a.id DESC LIMIT 50`,
// so the catalogue search box, the sort control and every page after the first
// 50 rows could not work. This file restores that parameter surface.
//
// Two rules hold everywhere below:
//
//   - No caller value is ever concatenated into SQL. Sort keys and directions
//     come out of a fixed allowlist and select a constant fragment; every other
//     value is a bind parameter. An unknown key is a 400, never a silent
//     fallback, so a client that misspells a sort key learns about it.
//   - The total is counted over the same FROM/WHERE the page is read from, so
//     `total` always describes the filtered set and not the whole catalogue.

// publicApplicationsSortColumns maps an accepted sort key to the SQL fragment
// that orders by it. The map is the allowlist: a key that is not here is
// refused. `likes` orders by the output column of the like-count subquery.
var publicApplicationsSortColumns = map[string]string{
	"id":         "a.id",
	"name":       "a.name",
	"created_at": "a.created_at",
	"likes":      "likes",
}

// publicApplicationsStatuses is the set of version statuses a caller may name
// in ?statuses=. The catalogue itself only ever exposes published versions, so
// the filter narrows that set; it cannot widen it.
var publicApplicationsStatuses = map[string]bool{
	"published":     true,
	"draft":         true,
	"on_moderation": true,
	"rejected":      true,
	"embedded":      true,
}

const (
	publicApplicationsDefaultLimit = 50
	publicApplicationsMaxLimit     = 1000
)

// publicApplicationsFilter is the parsed, validated query string.
type publicApplicationsFilter struct {
	Category   string
	Query      string
	Statuses   []string
	AgentsType string
	SortBy     string
	SortOrder  string
	Limit      int
	Offset     int
	MyLiked    bool
}

// publicApplicationsParamError names the query parameter a caller got wrong so
// the 400 body can point at it.
type publicApplicationsParamError struct {
	Param   string
	Message string
}

func (e *publicApplicationsParamError) Error() string { return e.Param + ": " + e.Message }

// parsePublicApplicationsFilter validates the query string. It returns a
// *publicApplicationsParamError for anything it refuses.
func parsePublicApplicationsFilter(values url.Values) (publicApplicationsFilter, error) {
	f := publicApplicationsFilter{
		Category:   strings.TrimSpace(values.Get("category")),
		Query:      strings.TrimSpace(values.Get("query")),
		AgentsType: strings.ToLower(strings.TrimSpace(values.Get("agents_type"))),
		SortBy:     strings.ToLower(strings.TrimSpace(values.Get("sort_by"))),
		SortOrder:  strings.ToLower(strings.TrimSpace(values.Get("sort_order"))),
		Limit:      publicApplicationsDefaultLimit,
		Offset:     0,
	}

	if f.SortBy == "" {
		// The pylon default. Rows created in one batch share a created_at, so
		// the id tiebreak below keeps the order stable and keeps the answer
		// identical to the old hard `ORDER BY a.id DESC` for such a batch.
		f.SortBy = "created_at"
	}
	if _, ok := publicApplicationsSortColumns[f.SortBy]; !ok {
		return f, &publicApplicationsParamError{
			Param:   "sort_by",
			Message: "must be one of id, name, created_at, likes",
		}
	}

	if f.SortOrder == "" {
		f.SortOrder = "desc"
	}
	if f.SortOrder != "asc" && f.SortOrder != "desc" {
		return f, &publicApplicationsParamError{Param: "sort_order", Message: "must be asc or desc"}
	}

	switch f.AgentsType {
	case "", "all", "classic", "pipeline":
	default:
		return f, &publicApplicationsParamError{
			Param:   "agents_type",
			Message: "must be one of all, classic, pipeline",
		}
	}

	if raw := strings.TrimSpace(values.Get("statuses")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			status := strings.ToLower(strings.TrimSpace(part))
			if status == "" {
				continue
			}
			if !publicApplicationsStatuses[status] {
				return f, &publicApplicationsParamError{
					Param:   "statuses",
					Message: "unknown status " + strconv.Quote(status),
				}
			}
			f.Statuses = append(f.Statuses, status)
		}
	}

	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > publicApplicationsMaxLimit {
			return f, &publicApplicationsParamError{
				Param:   "limit",
				Message: fmt.Sprintf("must be an integer between 1 and %d", publicApplicationsMaxLimit),
			}
		}
		f.Limit = limit
	}

	if raw := strings.TrimSpace(values.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return f, &publicApplicationsParamError{Param: "offset", Message: "must be a non-negative integer"}
		}
		f.Offset = offset
	}

	f.MyLiked = parseBoolParam(values.Get("my_liked"))

	return f, nil
}

// parseBoolParam reads the truthy spellings a browser query string carries.
// Anything else — including the empty string — is false, which matches the
// pylon default of `request.args.get('my_liked', False)`.
func parseBoolParam(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// publicApplicationsWhere builds the shared FROM/WHERE both the count and the
// page read from. `schema` is a quoted identifier produced by
// publicTenantSchema, never a caller value.
func publicApplicationsWhere(schema string, f publicApplicationsFilter, userID int, args *argList) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
		FROM %s.applications a
		JOIN %s.application_versions av ON av.application_id = a.id
		WHERE av.status = 'published'
		AND COALESCE(av.meta->>'status', '') != 'embedded'`, schema, schema)

	if f.Category != "" {
		if f.Category == "Other" {
			// "Other" is the catch-all: agents tagged Other and agents with no
			// category at all.
			b.WriteString(` AND (av.meta->>'category' IS NULL OR av.meta->>'category' = '' OR av.meta->>'category' = 'Other')`)
		} else {
			fmt.Fprintf(&b, ` AND av.meta->>'category' = %s`, args.add(f.Category))
		}
	}

	if len(f.Statuses) > 0 {
		// The catalogue only holds published versions, so this narrows the set.
		// A statuses list that leaves out "published" therefore answers nothing,
		// which is the honest reading of the filter.
		fmt.Fprintf(&b, ` AND av.status = ANY(%s)`, args.add(f.Statuses))
	}

	if f.Query != "" {
		// ILIKE over name and description, the pair pylon searches. The pattern
		// is a bind parameter, so a caller's % or _ only affects its own match.
		pattern := args.add("%" + f.Query + "%")
		fmt.Fprintf(&b, ` AND (a.name ILIKE %s OR COALESCE(a.description, '') ILIKE %s)`, pattern, pattern)
	}

	switch f.AgentsType {
	case "pipeline":
		b.WriteString(` AND av.agent_type = 'pipeline'`)
	case "classic":
		b.WriteString(` AND COALESCE(av.agent_type, '') != 'pipeline'`)
	}

	if f.MyLiked {
		if userID > 0 {
			fmt.Fprintf(&b, ` AND EXISTS (SELECT 1 FROM %s.social_likes ml
				WHERE ml.entity_name = 'application' AND ml.entity_id = a.id AND ml.user_id = %s)`,
				schema, args.add(userID))
		} else {
			// No caller identity means no likes of their own. Answering the whole
			// catalogue here would label a generic list "My Liked".
			b.WriteString(` AND FALSE`)
		}
	}

	return b.String()
}

// decodeJSONObject reads a jsonb column that holds an object. A malformed or
// empty column gives a nil map, which serialises as `null` — the same value the
// row carried before this file existed.
func decodeJSONObject(raw []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(raw, &out) // DB jsonb column; malformed means nil
	return out
}

// decodeJSONArray reads the aggregated tag list. A row with no tags gives an
// empty array, never null, so a client can iterate it without a guard.
func decodeJSONArray(raw []byte) []any {
	out := []any{}
	_ = json.Unmarshal(raw, &out) // DB-built json_agg text; malformed means empty
	if out == nil {
		out = []any{}
	}
	return out
}

// publicApplicationsOrderBy renders the ORDER BY from the allowlist. It never
// reads caller text: the key selects a constant fragment and the direction is
// one of two constants. The id tiebreak keeps paging stable when the sort key
// ties.
func publicApplicationsOrderBy(f publicApplicationsFilter) string {
	column, ok := publicApplicationsSortColumns[f.SortBy]
	if !ok {
		// Unreachable: parsePublicApplicationsFilter refuses unknown keys.
		column = "a.id"
	}
	direction := "DESC"
	if f.SortOrder == "asc" {
		direction = "ASC"
	}
	return fmt.Sprintf(" ORDER BY %s %s, a.id DESC", column, direction)
}

// PublicApplications lists the published applications of the public project.
func (h *Handler) PublicApplications(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}, "total": 0})
		return
	}
	ctx := r.Context()

	applicationID := chi.URLParam(r, "applicationID")
	if applicationID != "" {
		h.publicApplicationDetail(w, r, ctx, applicationID)
		return
	}

	filter, err := parsePublicApplicationsFilter(r.URL.Query())
	if err != nil {
		body := map[string]any{"error": "invalid_query_parameter"}
		var paramErr *publicApplicationsParamError
		if errors.As(err, &paramErr) {
			body["param"] = paramErr.Param
			body["msg"] = paramErr.Message
		}
		writeJSON(w, http.StatusBadRequest, body)
		return
	}

	publicProjectID := publicProjectIDOrDefault()
	schema := publicTenantSchema()

	// social_likes.user_id is an INTEGER column. auth.User.ID is a string, so
	// it is parsed here rather than bound as text: a principal whose id is not
	// a number (a service principal, say) has no likes of its own, and binding
	// its id would fail the whole read instead of answering "none".
	userID := 0
	if user, ok := auth.UserFromContext(ctx); ok {
		if parsed, err := strconv.Atoi(user.ID); err == nil {
			userID = parsed
		}
	}

	countArgs := &argList{}
	countSQL := "SELECT COUNT(*)" + publicApplicationsWhere(schema, filter, userID, countArgs)
	total := 0
	if err := h.pool.QueryRow(ctx, countSQL, countArgs.values...).Scan(&total); err != nil {
		slog.ErrorContext(ctx, "public_applications: count failed", "err", err)
	}

	pageArgs := &argList{}
	likesExpr := fmt.Sprintf(
		`(SELECT COUNT(*) FROM %s.social_likes sl WHERE sl.entity_name = 'application' AND sl.entity_id = a.id)`,
		schema)
	isLikedExpr := "FALSE"
	tagsExpr := fmt.Sprintf(`COALESCE((
			SELECT json_agg(json_build_object('id', t.id, 'name', t.name) ORDER BY t.name)
			FROM %s.application_version_tag_association ta
			JOIN %s.tags t ON t.id = ta.tag_id
			WHERE ta.version_id = av.id)::text, '[]')`, schema, schema)

	selectClause := fmt.Sprintf(`
		SELECT a.id, a.name, COALESCE(a.description, ''),
			av.id as version_id, av.name as version_name, av.agent_type,
			COALESCE(av.meta::text, '{}'),
			%s as tags,
			%s as likes,`, tagsExpr, likesExpr)
	where := publicApplicationsWhere(schema, filter, userID, pageArgs)
	if userID > 0 {
		isLikedExpr = fmt.Sprintf(
			`EXISTS (SELECT 1 FROM %s.social_likes sl2 WHERE sl2.entity_name = 'application' AND sl2.entity_id = a.id AND sl2.user_id = %s)`,
			schema, pageArgs.add(userID))
	}
	pageSQL := selectClause + " " + isLikedExpr + " as is_liked" + where +
		publicApplicationsOrderBy(filter) +
		" LIMIT " + pageArgs.add(filter.Limit) +
		" OFFSET " + pageArgs.add(filter.Offset)

	items := make([]map[string]any, 0)
	rows, err := h.pool.Query(ctx, pageSQL, pageArgs.values...)
	if err != nil {
		slog.ErrorContext(ctx, "public_applications: page read failed", "err", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var aID, vID, likes int
			var name, desc, vName, agentType string
			var metaJSON, tagsJSON []byte
			var isLiked bool
			if rows.Scan(&aID, &name, &desc, &vID, &vName, &agentType, &metaJSON, &tagsJSON, &likes, &isLiked) == nil {
				items = append(items, map[string]any{
					"project_id":   publicProjectID,
					"id":           strconv.Itoa(aID),
					"name":         name,
					"description":  desc,
					"version_id":   strconv.Itoa(vID),
					"version_name": vName,
					"agent_type":   agentType,
					"meta":         decodeJSONObject(metaJSON),
					"tags":         decodeJSONArray(tagsJSON),
					"likes":        likes,
					"is_liked":     isLiked,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"rows": items, "total": total})
}

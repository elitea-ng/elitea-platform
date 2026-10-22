package eliteacore

// The admin dashboard's PUBLISHED AGENTS listing —
// `GET /elitea_core/admin_published_agents/administration`.
//
// Legacy: legacy/plugins/elitea_core/api/v2/admin_published_agents.py. It reads
// the PUBLIC project's schema, keeps only applications that have at least one
// `published` version, and reports each one with its published versions.
//
// This is not the catalogue the product's own Agents page reads. It is the
// operator's view: every agent this deployment has published, who published
// each version and when, ordered newest first.
//
// One deliberate divergence, and it is the point of the endpoint. The reference
// reports adoption as
//
//	'adoption': {'conversation_count': adoption.get('conversation_count', 0),
//	             'project_count':      adoption.get('project_count', 0)}
//
// where `adoption` is `application.meta['adoption']` — a key nothing in either
// stack writes. So the reference dashboard renders "0 conversations, 0
// projects" for every agent, which reads as "nobody uses any of these" rather
// than "this platform does not measure that". Here the counters are null when
// the key is absent, and carry the stored numbers when a producer has written
// them. A dashboard can render "—" for the first and a number for the second;
// it cannot tell them apart from two zeroes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// PublishedAgentsListPermission is the permission the legacy original declares.
const PublishedAgentsListPermission = "runtime.admin.published_agents"

// maxPublishedAgentsPageSize bounds `?page_size`, so a caller cannot ask for
// the whole catalogue in one response.
const maxPublishedAgentsPageSize = 100

type publishedAgentVersion struct {
	VersionID   int     `json:"version_id"`
	VersionName string  `json:"version_name"`
	PublishedAt *string `json:"published_at"`
	PublishedBy any     `json:"published_by"`
}

type publishedAgentAdoption struct {
	// Pointers, not ints: see this file's header. `null` is "this platform does
	// not measure that", `0` is "measured, and it is zero".
	ConversationCount *json.Number `json:"conversation_count"`
	ProjectCount      *json.Number `json:"project_count"`
}

type publishedAgent struct {
	PublicAgentID          int                     `json:"public_agent_id"`
	Name                   string                  `json:"name"`
	Description            string                  `json:"description"`
	AuthorProjectID        *int                    `json:"author_project_id"`
	PublishedVersions      []publishedAgentVersion `json:"published_versions"`
	TotalPublishedVersions int                     `json:"total_published_versions"`
	Adoption               publishedAgentAdoption  `json:"adoption"`
	CreatedAt              *string                 `json:"created_at"`
}

// AdminPublishedAgents serves the listing.
func (h *Handler) AdminPublishedAgents(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}
	ctx := r.Context()

	page := positiveQueryInt(r, "page", 1)
	pageSize := positiveQueryInt(r, "page_size", 20)
	if pageSize > maxPublishedAgentsPageSize {
		pageSize = maxPublishedAgentsPageSize
	}
	// `created_at` is the reference's default and its only alternative is
	// `name`; anything else falls back rather than being interpolated.
	orderColumn := "app.created_at"
	if r.URL.Query().Get("sort") == "name" {
		orderColumn = "app.name"
	}

	schema := publicTenantSchema()
	publishedPredicate := fmt.Sprintf(`EXISTS (
    SELECT 1 FROM %s.application_versions version
    WHERE version.application_id = app.id AND version.status = 'published')`, schema)

	var total int
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.applications app WHERE %s`, schema, publishedPredicate),
	).Scan(&total); err != nil {
		// A read failure is reported as one. An empty page here would be
		// indistinguishable from "this deployment has published nothing".
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to list published agents"})
		return
	}

	items := make([]publishedAgent, 0)
	if total > 0 {
		var err error
		items, err = h.readPublishedAgents(r, schema, publishedPredicate, orderColumn, pageSize, (page-1)*pageSize)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"error": "failed to list published agents"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (h *Handler) readPublishedAgents(
	r *http.Request, schema, publishedPredicate, orderColumn string, limit, offset int,
) ([]publishedAgent, error) {
	ctx := r.Context()
	// One statement, versions aggregated in the database. The reference issues
	// a per-application `selectinload` and then filters the versions in Python,
	// which is where its published-only rule can silently drift from the one
	// the count uses.
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
SELECT app.id,
       app.name,
       COALESCE(app.description, ''),
       app.shared_owner_id,
       app.created_at,
       COALESCE(app.meta::text, '{}'),
       COALESCE(
           (SELECT json_agg(json_build_object(
                       'version_id', version.id,
                       'version_name', version.name,
                       'published_at', to_char(version.created_at, 'YYYY-MM-DD"T"HH24:MI:SS'),
                       'published_by', version.meta -> 'published_by')
                   ORDER BY version.created_at DESC, version.id DESC)
            FROM %[1]s.application_versions version
            WHERE version.application_id = app.id AND version.status = 'published')::text,
           '[]')
FROM %[1]s.applications app
WHERE %[2]s
ORDER BY %[3]s DESC, app.id DESC
LIMIT $1 OFFSET $2`, schema, publishedPredicate, orderColumn), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// No capacity hint. `limit` is bounded by maxPublishedAgentsPageSize before
	// it ever gets here, but sizing the allocation from a value that originates
	// in the query string is what CodeQL's uncontrolled-allocation-size rule
	// objects to, and it does not accept the clamp as a barrier. The page is at
	// most 100 rows, so pre-sizing buys nothing worth arguing with a scanner
	// over. It stays non-nil so an empty page marshals as `[]`, not `null`.
	agents := make([]publishedAgent, 0)
	for rows.Next() {
		var (
			agent      publishedAgent
			createdAt  *time.Time
			metaJSON   string
			versionRaw string
		)
		if err := rows.Scan(
			&agent.PublicAgentID, &agent.Name, &agent.Description,
			&agent.AuthorProjectID, &createdAt, &metaJSON, &versionRaw,
		); err != nil {
			return nil, err
		}
		if createdAt != nil {
			formatted := createdAt.Format("2006-01-02T15:04:05")
			agent.CreatedAt = &formatted
		}
		agent.PublishedVersions = []publishedAgentVersion{}
		if err := json.Unmarshal([]byte(versionRaw), &agent.PublishedVersions); err != nil {
			return nil, err
		}
		agent.TotalPublishedVersions = len(agent.PublishedVersions)
		agent.Adoption = readAdoption(metaJSON)
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// readAdoption pulls `meta.adoption` without inventing zeroes for it.
func readAdoption(metaJSON string) publishedAgentAdoption {
	var meta struct {
		Adoption *struct {
			ConversationCount *json.Number `json:"conversation_count"`
			ProjectCount      *json.Number `json:"project_count"`
		} `json:"adoption"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil || meta.Adoption == nil {
		return publishedAgentAdoption{}
	}
	return publishedAgentAdoption{
		ConversationCount: meta.Adoption.ConversationCount,
		ProjectCount:      meta.Adoption.ProjectCount,
	}
}

func positiveQueryInt(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

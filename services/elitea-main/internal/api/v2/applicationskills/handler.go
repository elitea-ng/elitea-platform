package applicationskills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CurrentApplicationSkillsPath       = "/api/v2/elitea_core/application_skills/prompt_lib/{projectID}/{appVersionID}"
	CurrentApplicationSkillsMode       = auth.PermissionModeDefault
	CurrentApplicationSkillsPermission = "models.applications.applications.details"
	MaxCurrentApplicationSkills        = 5
)

const (
	currentApplicationSkillsRoutePath     = "/api/v2/elitea_core/application_skills/prompt_lib/{projectID:[0-9]+}/{appVersionID:[0-9]+}"
	maxCurrentApplicationSkillsPostgresID = "2147483647"
)

var (
	ErrInvalidCurrentApplicationSkillsRoute   = errors.New("invalid current application-skills route dependencies")
	ErrInvalidCurrentApplicationSkillsRequest = errors.New("current application-skills request is invalid")
)

// CurrentApplicationSkill is the exact presentation shape returned by the
// current application_skills endpoint. Current schemas constrain VersionID,
// but a pointer preserves the Python handler's defensive missing-version
// projection without weakening the PostgreSQL fixture.
type CurrentApplicationSkill struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	SkillID        int32           `json:"skill_id"`
	VersionID      *int32          `json:"version_id"`
	VersionName    string          `json:"version_name"`
	VersionMissing bool            `json:"version_missing"`
	IconMeta       json.RawMessage `json:"icon_meta"`
	// CreatedAt carries skills.created_at for the published `items` half of
	// the envelope. It is `json:"-"` on purpose: the Pylon dict has no such
	// key, and adding one would break the byte parity the rest of this type
	// exists to hold.
	CreatedAt time.Time `json:"-"`
	// Instructions serve internal MCP without changing the public list body.
	Instructions string `json:"-"`
}

type CurrentApplicationSkillsReader interface {
	ListCurrentApplicationSkills(
		context.Context,
		int32,
		int32,
	) ([]CurrentApplicationSkill, error)
}

// CurrentApplicationSkillsRepository reads one already-authorized tenant
// through a transaction-local search_path. Caller-provided schema names never
// enter the query.
type CurrentApplicationSkillsRepository struct {
	tenants *tenant.Executor
}

func NewCurrentApplicationSkillsRepository(
	pool *pgxpool.Pool,
) (*CurrentApplicationSkillsRepository, error) {
	if pool == nil {
		return nil, errors.New("current application-skills database is required")
	}
	return &CurrentApplicationSkillsRepository{tenants: tenant.NewExecutor(pool)}, nil
}

func (repository *CurrentApplicationSkillsRepository) ListCurrentApplicationSkills(
	ctx context.Context,
	projectID int32,
	appVersionID int32,
) ([]CurrentApplicationSkill, error) {
	if repository == nil || repository.tenants == nil || ctx == nil ||
		projectID <= 0 || appVersionID <= 0 {
		return nil, ErrInvalidCurrentApplicationSkillsRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	skills := []CurrentApplicationSkill{}
	err := repository.tenants.WithinTx(
		ctx,
		tenant.Project{ID: int64(projectID)},
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
SELECT
    skill.name,
    skill.description,
    mapping.skill_id,
    mapping.skill_version_id,
    CASE WHEN version.id IS NULL THEN 'unknown' ELSE version.name END,
    version.id IS NULL,
    CASE
        WHEN version.id IS NULL THEN 'null'::jsonb
        ELSE COALESCE(version.meta -> 'icon_meta', 'null'::jsonb)
    END,
    skill.created_at,
    COALESCE(version.instructions, '')
FROM entity_skill_mapping AS mapping
JOIN skills AS skill
  ON skill.id = mapping.skill_id
LEFT JOIN skill_versions AS version
  ON version.id = mapping.skill_version_id
WHERE mapping.entity_version_id = $1
  AND mapping.entity_type = 'agent'`,
				appVersionID,
			)
			if err != nil {
				return fmt.Errorf("list current application skills: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var skill CurrentApplicationSkill
				var iconMeta []byte
				if err := rows.Scan(
					&skill.Name,
					&skill.Description,
					&skill.SkillID,
					&skill.VersionID,
					&skill.VersionName,
					&skill.VersionMissing,
					&iconMeta,
					&skill.CreatedAt,
					&skill.Instructions,
				); err != nil {
					return fmt.Errorf("scan current application skill: %w", err)
				}
				skill.IconMeta = json.RawMessage(bytes.Clone(iconMeta))
				skills = append(skills, skill)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate current application skills: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return skills, nil
}

var _ CurrentApplicationSkillsReader = (*CurrentApplicationSkillsRepository)(nil)

// CurrentApplicationSkillsRoute owns only the current attached-skills GET and
// remains unmounted until production composition explicitly selects the slice.
type CurrentApplicationSkillsRoute struct {
	handler http.Handler
}

func NewCurrentApplicationSkillsRoute(
	reader CurrentApplicationSkillsReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentApplicationSkillsRoute, error) {
	// ForwardedIdentityVerifier is OPTIONAL, PrincipalValidator is not. See
	// internal/api/v2/indextypes/handler.go for the full reason: only the Form
	// plane builds a verifier, an OIDC or SAML install authenticates the same
	// caller through a cookie or a bearer token, and this route is the ONLY
	// handler for its path now that the prototype fallback is deleted (#395).
	if reader == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidCurrentApplicationSkillsRoute
	}

	handler := &currentApplicationSkillsHandler{reader: reader}
	endpoint := http.Handler(http.HandlerFunc(handler.list))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationSkillsMode,
		currentApplicationSkillsProjectID,
		CurrentApplicationSkillsPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, currentApplicationSkillsRoutePath, endpoint)
	router.NotFound(writeCurrentApplicationSkillsNotFound)
	return &CurrentApplicationSkillsRoute{handler: router}, nil
}

func (route *CurrentApplicationSkillsRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentApplicationSkillsHandler struct {
	reader CurrentApplicationSkillsReader
}

// currentApplicationSkillsResponse answers BOTH shipped clients from one body.
//
// The Pylon keys `skills` and `max_skills` are unchanged, byte for byte:
// apps/elitea-ui reads them (features/skill/ui/ApplicationSkills.jsx and the
// two mention hooks read `data.skills` and `data.max_skills`), and the edge
// cutover this slice was written for depends on them.
//
// The `items`/`total`/`page`/`page_size`/`total_pages` keys are the PUBLISHED
// contract for this path — SkillsList in api/openapi/v2.yaml. apps/elitea-web
// reads that half: shared/api/unwrap.ts takes `items` first, and both skill
// mention hooks go through it. Before this envelope carried them, turning
// ELITEA_APPLICATION_SKILLS_ENABLED on gave the web client a body with no
// `items` key, which unwrapList reports as an unrecognised shape and renders
// as "no skills" (#395). That is why the flag could not be turned on.
//
// The two halves project the SAME rows, so they cannot disagree. Extra keys
// are contract-legal: SkillsList does not close its object, so a client
// generated from the published spec ignores what it did not ask for.
type currentApplicationSkillsResponse struct {
	Items      []v2skills.Skill `json:"items"`
	Total      int              `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalPages int              `json:"total_pages"`

	Skills    []CurrentApplicationSkill `json:"skills"`
	MaxSkills int                       `json:"max_skills"`
}

// currentApplicationSkillType is the value the skills List handler marshals for
// every row (internal/infra/db/repos/skills.go scanSkillRow). The published
// Skill schema makes `type` required, so the same literal is used here rather
// than an empty string.
const currentApplicationSkillType = "skill"

// newCurrentApplicationSkillsResponse builds both halves from one row set.
//
// The pagination numbers are one page, sized by the attached set. They copied
// SkillsRepo.ListForApplicationVersion, the prototype handler that served this
// path where the capability was off, so the same request got the same body from
// either handler. #395 deleted that handler; the numbers stay, because a client
// that read them from the old route must keep reading the same values.
//
// Only the fields the published Skill schema requires, plus `description`, are
// filled. `instructions`, `tags` and `versions` stay absent, because this read
// projects the ATTACHED skill version, not the base version those fields come
// from; inventing them from the attached version would put a different
// version's identity behind a base-version key. `is_default` and `updated_at`
// take the same degenerate values the skills List handler already ships.
func newCurrentApplicationSkillsResponse(
	projectID string,
	attached []CurrentApplicationSkill,
) currentApplicationSkillsResponse {
	items := make([]v2skills.Skill, 0, len(attached))
	for _, skill := range attached {
		items = append(items, v2skills.Skill{
			ID:          strconv.FormatInt(int64(skill.SkillID), 10),
			ProjectID:   projectID,
			Name:        skill.Name,
			Description: skill.Description,
			Type:        currentApplicationSkillType,
			CreatedAt:   skill.CreatedAt,
		})
	}
	totalPages := 0
	if len(items) > 0 {
		totalPages = 1
	}
	return currentApplicationSkillsResponse{
		Items:      items,
		Total:      len(items),
		Page:       1,
		PageSize:   len(items),
		TotalPages: totalPages,
		Skills:     append([]CurrentApplicationSkill{}, attached...),
		MaxSkills:  MaxCurrentApplicationSkills,
	}
}

func (handler *currentApplicationSkillsHandler) list(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, projectOK := currentApplicationSkillsPostgresID(
		chi.URLParam(request, "projectID"),
	)
	appVersionID, versionOK := currentApplicationSkillsPostgresID(
		chi.URLParam(request, "appVersionID"),
	)
	if !projectOK || !versionOK {
		writeCurrentApplicationSkillsJSON(
			writer,
			http.StatusOK,
			newCurrentApplicationSkillsResponse("", nil),
		)
		return
	}

	skills, err := handler.reader.ListCurrentApplicationSkills(
		request.Context(),
		projectID,
		appVersionID,
	)
	if err != nil {
		writeCurrentApplicationSkillsFailure(writer)
		return
	}

	writeCurrentApplicationSkillsJSON(
		writer,
		http.StatusOK,
		newCurrentApplicationSkillsResponse(
			strconv.FormatInt(int64(projectID), 10),
			skills,
		),
	)
}

func currentApplicationSkillsProjectID(request *http.Request) (string, bool) {
	projectID, ok := currentApplicationSkillsDecimal(
		chi.URLParam(request, "projectID"),
	)
	if !ok {
		return "", false
	}
	return projectID, true
}

func currentApplicationSkillsDecimal(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	normalized := strings.TrimLeft(value, "0")
	if normalized == "" {
		return "0", true
	}
	return normalized, true
}

func currentApplicationSkillsPostgresID(value string) (int32, bool) {
	normalized, ok := currentApplicationSkillsDecimal(value)
	if !ok || normalized == "0" ||
		len(normalized) > len(maxCurrentApplicationSkillsPostgresID) ||
		(len(normalized) == len(maxCurrentApplicationSkillsPostgresID) &&
			normalized > maxCurrentApplicationSkillsPostgresID) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(normalized, 10, 32)
	return int32(parsed), err == nil
}

func writeCurrentApplicationSkillsFailure(writer http.ResponseWriter) {
	writeCurrentApplicationSkillsJSON(writer, http.StatusInternalServerError, map[string]string{
		"message": "Internal Server Error",
	})
}

func writeCurrentApplicationSkillsNotFound(
	writer http.ResponseWriter,
	_ *http.Request,
) {
	writeCurrentApplicationSkillsJSON(writer, http.StatusNotFound, map[string]string{
		"message": "The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.",
	})
}

func writeCurrentApplicationSkillsJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

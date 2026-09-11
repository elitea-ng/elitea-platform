package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// SkillVersion holds one independently selected skill version.
type SkillVersion struct {
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name"`
	Instructions string   `json:"instructions"`
	Tags         []string `json:"tags"`
	// Meta is skill_versions.meta. It carries `icon_meta`, the {name,url}
	// pair the skill icon routes write
	// (internal/api/v2/eliteacore/skill_icon.go).
	//
	// WITHOUT THIS FIELD the icon is written and never read. The web client
	// renders `version_details.meta.icon_meta` — it is the shape the old
	// app's own optimistic update patches — so a read path that drops `meta`
	// turns a working write into an invisible one: the PUT answers
	// `{"updated": true}`, the row holds the icon, and the form still shows
	// the placeholder. Omitted when the column is NULL or `{}`.
	Meta map[string]any `json:"meta,omitempty"`
	// Status is skill_versions.status: "draft" or "published" (#249). A
	// published version is frozen — UpdateVersion and DeleteVersion both
	// refuse it, mirroring application_versions' own published/embedded
	// guard (skills.go's guardEntityVersion already applies this rule to
	// entity_skill_mapping writes; #874 applies the same status read to
	// version content writes).
	Status string `json:"status,omitempty"`
	// CreatedAt is skill_versions.created_at, surfaced so the version
	// selector can sort newest-first the way AgentPipelineVersionSelector
	// does.
	CreatedAt time.Time `json:"created_at,omitempty"`
	// ParentVersionID is skill_versions.parent_version_id
	// (migrations/tenant/0136_skill_version_lineage.sql): which version this
	// one was cloned or restored from. Omitted when the row has no recorded
	// ancestor — every skill's original `base` version, and any version
	// created before this migration ran.
	ParentVersionID string `json:"parent_version_id,omitempty"`
	// IsDefault reports whether this is the version named by
	// skills.meta.default_version_id — the version a new attachment
	// proposes and the one skillpublish's ExportFork prefers when nothing
	// else pins one. Mirrors ApplicationVersionDetail's own `is_default`.
	IsDefault bool `json:"is_default,omitempty"`
}

type Skill struct {
	// AuthorID comes from the authenticated principal, never the request body.
	AuthorID int64 `json:"-"`
	// MetadataOnly leaves version content unchanged during a metadata update.
	MetadataOnly bool           `json:"-"`
	Meta         map[string]any `json:"meta,omitempty"`
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Type         string         `json:"type"`
	Config       map[string]any `json:"config,omitempty"`
	IsDefault    bool           `json:"is_default"`
	// Instructions/Tags mirror the selected version's content at the top level
	// for convenience; Versions/VersionDetails carry the same data in the
	// shape the frontend actually reads (skill.version_details ?? skill.versions[0]).
	Instructions   string         `json:"instructions,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Versions       []SkillVersion `json:"versions,omitempty"`
	VersionDetails *SkillVersion  `json:"version_details,omitempty"`
	// DefaultVersionID is skills.meta->>'default_version_id' (#874),
	// mirroring applications.meta->>'default_version_id'
	// (repos/applications.go). It is the version a new attachment proposes;
	// it does NOT affect which version a chat turn reads — that stays keyed
	// off entity_skill_mapping.skill_version_id, fixed at attach time.
	DefaultVersionID string    `json:"default_version_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// skillVersionInput lets Create accept the {versions: [{name, instructions, tags}]}
// shape the frontend's createSkill() sends, distinct from Update's flat shape.
type skillVersionInput struct {
	Name         string   `json:"name,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

type createRequest struct {
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Instructions string              `json:"instructions,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
	Versions     []skillVersionInput `json:"versions,omitempty"`
}

func (r createRequest) toSkill() Skill {
	sk := Skill{Name: r.Name, Description: r.Description, Instructions: r.Instructions, Tags: r.Tags}
	if len(r.Versions) > 0 {
		if sk.Instructions == "" {
			sk.Instructions = r.Versions[0].Instructions
		}
		if sk.Tags == nil {
			sk.Tags = r.Versions[0].Tags
		}
	}
	return sk
}

type ListParams struct {
	Limit     int
	Offset    int
	IDs       []int64
	TagIDs    []int64
	AuthorID  int64
	Statuses  []string
	Page      int
	PageSize  int
	Query     string
	SortBy    string
	SortOrder string
}

type ListResponse struct {
	Items      []Skill `json:"items"`
	Total      int     `json:"total"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	TotalPages int     `json:"total_pages"`
}

// SkillEntityTypeAgent is the only entity type that can carry a skill.
//
// Pylon closes the set to one member: `SkillEntityTypes` has `agent` and
// nothing else (legacy/plugins/elitea_core/models/enums/all.py:72-74), and the
// request model types the field as that enum
// (legacy/plugins/elitea_core/models/pd/skill.py:355), so "pipeline" is a 400
// there. Both read paths filter on this literal — the chat read
// (internal/db/queries/agent_chat.sql:132) and the attached-skill registry
// (internal/api/v2/applications/handler.go:1398) — so a row written with any
// other value is a row nothing reads.
const SkillEntityTypeAgent = "agent"

// MaxSkillsPerEntityVersion caps the skills one agent version may carry.
//
// It is pylon's MAX_SKILLS_PER_AGENT
// (legacy/plugins/elitea_core/utils/skill_utils.py:31). The read side already
// publishes the same number: applicationskills.MaxCurrentApplicationSkills,
// which the old skill picker renders as "n/5 skills added" and uses to disable
// the menu. Without the same cap on the write side the counter can show 6/5.
const MaxSkillsPerEntityVersion = 5

// SkillRelation is one row of entity_skill_mapping, as the relation form of
// PATCH /skill/{mode}/{projectID}/{skillID} names it.
//
// The table has NO entity_id column (001_initial.sql:422-431). Its key is
// (entity_version_id, skill_id, entity_type), and the skill id comes from the
// path, so these three fields plus the path segment address one row exactly.
type SkillRelation struct {
	// EntityVersionID is application_versions.id, not applications.id.
	EntityVersionID string
	// EntityType is "agent" when the request does not name one.
	EntityType string
	// SkillVersionID names the skill version the attachment serves. Attach
	// requires it; detach does not read it, because it is not part of the key.
	SkillVersionID string
}

// SkillAttachment is the attach response body.
//
// It is pylon's four-key dict, verbatim
// (legacy/plugins/elitea_core/utils/skill_utils.py:1228-1233). The ids are
// numbers there, so they are numbers here.
type SkillAttachment struct {
	SkillID        int    `json:"skill_id"`
	SkillVersionID int    `json:"skill_version_id"`
	SkillName      string `json:"skill_name"`
	VersionName    string `json:"version_name"`
}

type Repository interface {
	List(ctx context.Context, projectID string, params ListParams) (ListResponse, error)
	// NOTE(#395): ListForApplicationVersion was declared here. It served
	// GET /application_skills/{mode}/{projectID}/{appVersionID}, the PROTOTYPE
	// fallback for the attached-skills read, and #395 deleted that route.
	// internal/api/v2/applicationskills owns the read now, through its own
	// tenant-scoped repository.
	//
	// AttachSkill and DetachSkill own the entity_skill_mapping row that read
	// looks at. They stay on this interface: a repository that cannot write the
	// attachment does not compile.
	AttachSkill(ctx context.Context, projectID, skillID string, relation SkillRelation) (SkillAttachment, error)
	DetachSkill(ctx context.Context, projectID, skillID string, relation SkillRelation) error
	Get(ctx context.Context, projectID, skillID string) (Skill, error)
	GetByName(ctx context.Context, projectID, name string) (Skill, bool, error)
	Create(ctx context.Context, projectID string, skill Skill) (Skill, error)
	Update(ctx context.Context, projectID, skillID string, skill Skill) (Skill, error)
	Delete(ctx context.Context, projectID, skillID string) error

	// ---- Version machinery (#874) --------------------------------------
	//
	// The unversioned Get/Update/Delete above stay exactly as they were —
	// they always read and write the skill's `base` version, the same
	// version a skill has always had. These five methods are what a skill
	// gained: multiple NAMED versions alongside `base`, get/create/update/
	// delete over that set, and a rollback that copies a named version's
	// content back onto `base`.
	//
	// There is no separate ListVersions: Get/GetVersion already carry the
	// FULL Versions slice (every version, not just `base`, since #874) — the
	// version selector and compare UI's "list versions" need is served by
	// the same read that already answers the skill's detail view, matching
	// features/skills/api/skillsApi.ts's contract
	// (apps/elitea-web/src/shared/api/endpoints.manifest.json's
	// skills.getSkill/getSkillVersion entries; there is no
	// skills.listVersions entry to serve).

	// GetVersion returns the skill with VersionDetails/Instructions/Tags set
	// to the NAMED version (not necessarily `base`), and Versions carrying
	// every version — the shape the version-switch and compare UI need.
	// Answers NotFound when versionID does not belong to skillID.
	GetVersion(ctx context.Context, projectID, skillID, versionID string) (Skill, error)
	// CreateVersion adds one NAMED version ("Save As Version"). `Name` must
	// be non-empty and not "base" — the handler refuses those before this is
	// called — and a duplicate name is a Conflict. When Instructions is
	// empty, the new version clones SourceVersionID's content (default:
	// `base`), matching entity-versioning.mdx's "Save As Version" for
	// agents/pipelines; the frontend's existing createSkillVersion() call
	// already sends the full instructions/tags of whatever is on screen, so
	// the common path never takes the clone branch.
	CreateVersion(ctx context.Context, projectID, skillID string, input VersionCreateInput) (Skill, error)
	// UpdateVersion edits ONE named version's instructions/tags (plus the
	// skill's own name/description, shared across every version, same as
	// Update). Refuses a published version with Conflict, mirroring
	// application_versions' "Unpublish first" guard.
	UpdateVersion(ctx context.Context, projectID, skillID, versionID string, skill Skill) (Skill, error)
	// DeleteVersion removes one named version. Refuses `base` and the
	// current default version with BadRequest, and a published version with
	// Conflict.
	DeleteVersion(ctx context.Context, projectID, skillID, versionID string) error
	// RestoreVersion is the rollback: it copies versionID's
	// instructions/tags/meta onto `base` and records the lineage
	// (parent_version_id), then returns the skill with `base` as
	// VersionDetails. Unlike agents' "Set as default" — which repoints a
	// pointer and leaves every version's content untouched — skills have no
	// distinguished "currently active" row to repoint: entity_skill_mapping
	// pins a skill_version_id at ATTACH time (skills.go's AttachSkill), so
	// restoring a version means overwriting the one row every unversioned
	// read and every new attachment actually uses.
	RestoreVersion(ctx context.Context, projectID, skillID, versionID string) (Skill, error)
	// SetDefaultVersion writes skills.meta.default_version_id. It does not
	// change which version any existing agent attachment resolves — see
	// Skill.DefaultVersionID's doc comment.
	SetDefaultVersion(ctx context.Context, projectID, skillID, versionID string) (Skill, error)
}

// VersionCreateInput is CreateVersion's request shape.
type VersionCreateInput struct {
	AuthorID     int64
	Name         string
	Instructions string
	Tags         []string
	// SourceVersionID names the version to clone Instructions/Tags from when
	// both are empty. Empty means `base`.
	SourceVersionID string
}

type Handler struct {
	repo Repository
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{skillID}", h.Get)
	r.Get("/{skillID}/{versionID}", h.Get)
	r.Put("/{skillID}", h.Update)
	r.Put("/{skillID}/{versionID}", h.Update)
	r.Delete("/{skillID}", h.Delete)
	r.Delete("/{skillID}/{versionID}", h.Delete)
	r.Post("/{skillID}/versions", h.CreateVersion)
	r.Post("/{skillID}/versions/{versionID}/restore", h.RestoreVersion)
	r.Patch("/{skillID}/default_version", h.SetDefaultVersion)
	return r
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	params := ListParams{
		Page:      page,
		PageSize:  pageSize,
		Query:     strings.TrimSpace(r.URL.Query().Get("query")),
		SortBy:    r.URL.Query().Get("sort_by"),
		SortOrder: r.URL.Query().Get("sort_order"),
	}

	resp, err := h.repo.List(r.Context(), projectID, params)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// NOTE(#395): `func (h *Handler) ListForApplication` and its helper
// `isPositiveInteger` stood here, mounted at
// GET /application_skills/{mode}/{projectID}/{appVersionID}.
//
// It was the PROTOTYPE fallback for the attached-skills read. The route
// pointed at List until #367; List never reads {appVersionID}, so opening any
// agent version answered with every skill in the project, at 200, in the same
// envelope. #393 pointed the mount at this handler, and #395 deletes the mount
// and the handler.
//
// internal/api/v2/applicationskills answers this path now. It reads
// entity_skill_mapping through a transaction-local tenant search_path and
// carries the published SkillsList keys beside the Pylon ones, so both shipped
// clients accept one body.

// Get selects the requested version or the configured default.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")
	versionID := chi.URLParam(r, "versionID")

	var skill Skill
	var err error
	if versionID != "" {
		skill, err = h.repo.GetVersion(r.Context(), projectID, skillID, versionID)
	} else {
		skill, err = h.repo.Get(r.Context(), projectID, skillID)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, skill)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	authorID, ok := skillAuthor(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("authenticated skill author is required"))
		return
	}
	skill := req.toSkill()
	skill.AuthorID = authorID
	created, err := h.repo.Create(r.Context(), projectID, skill)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// createVersionRequest is CreateVersion's body: `{name, instructions, tags}`,
// the exact shape features/skills/api/skillsApi.ts's createSkillVersion()
// already sends. SourceVersionID is new surface: when a caller omits
// Instructions, the new version clones SourceVersionID's content (default
// `base`) rather than being created empty.
type createVersionRequest struct {
	Name            string   `json:"name"`
	Instructions    string   `json:"instructions,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	SourceVersionID string   `json:"source_version_id,omitempty"`
}

// CreateVersion serves POST /skill/{mode}/{projectID}/{skillID} — the route
// that used to be bound to Create and silently created an unrelated new
// skill instead of a version of this one (see router.go's mount comment).
func (h *Handler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")

	var req createVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		apierr.Write(w, apierr.BadRequest("version name is required"))
		return
	}
	if name == "base" {
		apierr.Write(w, apierr.BadRequest(`"base" is reserved and cannot be used as a version name`))
		return
	}

	authorID, ok := skillAuthor(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("authenticated skill author is required"))
		return
	}
	created, err := h.repo.CreateVersion(r.Context(), projectID, skillID, VersionCreateInput{
		AuthorID:        authorID,
		Name:            name,
		Instructions:    req.Instructions,
		Tags:            req.Tags,
		SourceVersionID: req.SourceVersionID,
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// RestoreVersion serves POST
// /skill_version_restore/{mode}/{projectID}/{skillID}/{versionID} — the
// rollback the issue asks for. It copies versionID's content back onto
// `base` and returns the skill with `base` as VersionDetails.
func (h *Handler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")
	versionID := chi.URLParam(r, "versionID")

	restored, err := h.repo.RestoreVersion(r.Context(), projectID, skillID, versionID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, restored)
}

// SetDefaultVersion serves PATCH
// /skill_default_version/{mode}/{projectID}/{skillID}. Before #874 this URL
// was bound to the generic Update, which decodes {name, description,
// instructions, tags} and reads no "version_id" key — the frontend's
// setDefaultSkillVersion() has sent {"version_id": N} here since before this
// change, Update saw no "name" key in that body, and wrote the skill's own
// name to "" on every "Set default" click. This reads version_id (numeric or
// string, same leniency as the skill-relation body) and writes
// skills.meta.default_version_id.
func (h *Handler) SetDefaultVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxUpdateBytes))
	if err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var keys map[string]json.RawMessage
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &keys); err != nil {
			apierr.Write(w, apierr.BadRequest("invalid request body"))
			return
		}
	}

	versionID, err := relationID(keys, "version_id")
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}
	if versionID == "" {
		apierr.Write(w, apierr.BadRequest("version_id is required"))
		return
	}

	updated, err := h.repo.SetDefaultVersion(r.Context(), projectID, skillID, versionID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// Update serves PUT and PATCH on /skill/{mode}/{projectID}/{skillID}.
//
// The URL is overloaded, and the body shape selects the operation. A body that
// carries a `has_relation` key attaches or detaches a skill; any other body
// updates the skill itself. This is the convention the old app already uses on
// this exact URL (apps/elitea-ui/src/[fsd]/features/skill/api/skillsApi.js:306-331,
// `updateSkillRelation`), and the convention the toolkit twin already
// implements in Go (internal/api/v2/toolkits/handler.go:826). A new route would
// leave the contract the frontend calls unserved.
//
// Before this change the relation body decoded into `createRequest`, which
// names none of its four keys. Every field was dropped, the skill's own name
// and description were overwritten with "", and the caller got 200. Nothing was
// attached.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")
	versionID := chi.URLParam(r, "versionID")

	// The body is read once and unmarshalled twice, because presence of a key
	// cannot be seen through `createRequest`.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpdateBytes))
	if err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if _, present := keys["has_relation"]; present {
		h.updateSkillRelation(w, r, projectID, skillID, keys)
		return
	}

	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	// #874: the {versionID}-scoped PUT edits a NAMED version's
	// instructions/tags instead of `base`. The skill's own name/description
	// still come from the same body — they are columns on `skills`, shared
	// across every version — so UpdateVersion writes both the shared row and
	// the one named version.
	var updated Skill
	if versionID != "" {
		updated, err = h.repo.UpdateVersion(r.Context(), projectID, skillID, versionID, req.toSkill())
	} else {
		updated, err = h.repo.Update(r.Context(), projectID, skillID, req.toSkill())
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// maxUpdateBytes bounds the body Update reads into memory before it can tell
// the two operations apart.
const maxUpdateBytes = 1 << 20 // 1MiB

// updateSkillRelation attaches or detaches one skill.
//
// Status codes and bodies follow pylon's `patch`
// (legacy/plugins/elitea_core/api/v2/skill.py:209-245): attach answers 201 with
// the four-key attachment, detach answers 200 with {"ok": true}.
func (h *Handler) updateSkillRelation(
	w http.ResponseWriter,
	r *http.Request,
	projectID, skillID string,
	body map[string]json.RawMessage,
) {
	skillID, err := rowID(skillID, "skill id")
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}

	// A non-boolean `has_relation` is refused, not coerced. The toolkit twin
	// reads it with a comma-ok assertion, so a string or a null there means
	// false, which means DETACH — a request that says nothing intelligible
	// deletes an attachment. The two directions of this route are not
	// symmetrical in cost, so an unreadable value gets no default.
	//
	// A JSON null needs its own refusal: encoding/json unmarshals null into a
	// bool as a no-op and reports no error, which would leave the false that
	// means detach.
	var hasRelation bool
	raw := body["has_relation"]
	if string(raw) == "null" || json.Unmarshal(raw, &hasRelation) != nil {
		apierr.Write(w, apierr.BadRequest("has_relation must be true or false"))
		return
	}

	entityVersionID, err := relationID(body, "entity_version_id")
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}
	if entityVersionID == "" {
		apierr.Write(w, apierr.BadRequest("entity_version_id is required"))
		return
	}

	entityType, err := relationEntityType(body)
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}

	relation := SkillRelation{EntityVersionID: entityVersionID, EntityType: entityType}

	if !hasRelation {
		if err := h.repo.DetachSkill(r.Context(), projectID, skillID, relation); err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	skillVersionID, err := relationID(body, "skill_version_id")
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}
	// Required on attach, and pylon says so in a model validator
	// (legacy/plugins/elitea_core/models/pd/skill.py:357-362). It is not
	// optional in practice either: both readers of the row LEFT JOIN
	// skill_versions through this column and serve
	// COALESCE(instructions, ''), and the registry then DROPS a skill whose
	// instructions are blank. An attachment with no skill version is therefore
	// a row that the agent run cannot see.
	if skillVersionID == "" {
		apierr.Write(w, apierr.BadRequest("skill_version_id is required when has_relation is true"))
		return
	}
	relation.SkillVersionID = skillVersionID

	attachment, err := h.repo.AttachSkill(r.Context(), projectID, skillID, relation)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, attachment)
}

// relationEntityType reads `entity_type`, which defaults to "agent".
func relationEntityType(body map[string]json.RawMessage) (string, error) {
	raw, present := body["entity_type"]
	if !present || string(raw) == "null" {
		return SkillEntityTypeAgent, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("entity_type must be %q", SkillEntityTypeAgent)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return SkillEntityTypeAgent, nil
	}
	if value != SkillEntityTypeAgent {
		return "", fmt.Errorf("entity_type must be %q", SkillEntityTypeAgent)
	}
	return value, nil
}

// relationID reads one id off the relation body and returns it as a decimal
// string, or "" when the key is absent or null.
//
// Both a JSON number and a JSON string are accepted. Pylon types these fields
// `int`, and pydantic coerces a numeric string to an int, so both shapes reach
// the same place there. Anything that is not a positive whole number is
// refused rather than coerced: these values address a row, and a coerced id
// addresses the wrong one.
func relationID(body map[string]json.RawMessage, key string) (string, error) {
	raw, present := body[key]
	if !present || string(raw) == "null" {
		return "", nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "", nil
		}
		return rowID(text, key)
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", fmt.Errorf("%s must be a positive integer", key)
	}
	return rowID(number.String(), key)
}

// rowID refuses an id that no row can carry, and returns it in canonical form.
//
// The columns are PostgreSQL INTEGER, so a value above 2147483647 has no row
// and reaches pgx as "value out of range" — a 500 for a request the caller got
// wrong. The bound turns that into a 400.
func rowID(value, key string) (string, error) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 {
		return "", fmt.Errorf("%s must be a positive integer", key)
	}
	return strconv.FormatInt(parsed, 10), nil
}

// Delete serves both DELETE /skill/{mode}/{projectID}/{skillID} (removes the
// whole skill) and the {versionID}-scoped form (#874, removes one named
// version — DeleteVersion refuses `base` and the current default).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")
	versionID := chi.URLParam(r, "versionID")

	var err error
	if versionID != "" {
		err = h.repo.DeleteVersion(r.Context(), projectID, skillID, versionID)
	} else {
		err = h.repo.Delete(r.Context(), projectID, skillID)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Import / Export ---------------------------------------------------------
//
// Both round-trip the same YAML-frontmatter-plus-markdown-body format the
// frontend's import wizard parses client-side for preview
// (apps/elitea-web parseMdFrontmatter / the old app's parseMdFrontmatter):
//
//	---
//	name: ...
//	description: ...
//	tags: [...]
//	---
//	<instructions body>

type skillFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
}

var frontmatterPattern = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

func serializeSkillMarkdown(sk Skill) (string, error) {
	fm := skillFrontmatter{Name: sk.Name, Description: sk.Description, Tags: sk.Tags}
	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return "", fmt.Errorf("render frontmatter: %w", err)
	}
	return "---\n" + string(yamlBytes) + "---\n" + sk.Instructions, nil
}

func parseSkillMarkdown(content string) (name, description, instructions string, tags []string, err error) {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(content, "---") {
		return "", "", "", nil, fmt.Errorf("file is missing required metadata: must start with a YAML frontmatter block (enclosed in ---)")
	}

	m := frontmatterPattern.FindStringSubmatch(content)
	if m == nil {
		return "", "", "", nil, fmt.Errorf("invalid md format: missing closing ---")
	}

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(m[1]), &fm); err != nil {
		return "", "", "", nil, fmt.Errorf("invalid frontmatter: %w", err)
	}
	if fm.Name == "" || fm.Description == "" {
		return "", "", "", nil, fmt.Errorf(`frontmatter must contain "name" and "description"`)
	}

	return fm.Name, fm.Description, strings.TrimSpace(m[2]), fm.Tags, nil
}

func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, ch := range strings.TrimSpace(name) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == ' ':
			b.WriteRune(ch)
		default:
			b.WriteRune('_')
		}
	}
	result := strings.TrimSpace(b.String())
	if result == "" {
		return "skill"
	}
	return result
}

// maxImportBytes bounds the multipart/JSON body accepted by Import.
const maxImportBytes = 10 << 20 // 10MiB

// Import accepts a .md file (multipart form field "file") OR a JSON
// {content, filename} body, matching skillsApi.ts/skillsApi.js's skillImport
// contract. Only .md is accepted; a duplicate skill name reuses the existing
// skill and returns a `notice` field instead of erroring.
func (h *Handler) Import(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	content, filename, err := readImportPayload(r)
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}

	if filename != "" && !strings.HasSuffix(strings.ToLower(filename), ".md") {
		apierr.Write(w, apierr.BadRequest("only .md files can be imported"))
		return
	}

	name, description, instructions, tags, err := parseSkillMarkdown(content)
	if err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}

	if existing, found, lookupErr := h.repo.GetByName(r.Context(), projectID, name); lookupErr == nil && found {
		writeJSON(w, http.StatusOK, skillWithNotice{
			Skill:  existing,
			Notice: fmt.Sprintf("A skill named %q already exists; reusing it.", name),
		})
		return
	}

	authorID, ok := skillAuthor(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("authenticated skill author is required"))
		return
	}
	created, err := h.repo.Create(r.Context(), projectID, Skill{
		AuthorID:     authorID,
		Name:         name,
		Description:  description,
		Instructions: instructions,
		Tags:         tags,
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

type skillWithNotice struct {
	Skill
	Notice string `json:"notice"`
}

func readImportPayload(r *http.Request) (content, filename string, err error) {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/") {
		if parseErr := r.ParseMultipartForm(maxImportBytes); parseErr != nil {
			return "", "", fmt.Errorf("invalid multipart form: %w", parseErr)
		}
		file, header, formErr := r.FormFile("file")
		if formErr == nil {
			defer func() { _ = file.Close() }()
			body, readErr := io.ReadAll(io.LimitReader(file, maxImportBytes))
			if readErr != nil {
				return "", "", fmt.Errorf("failed to read uploaded file: %w", readErr)
			}
			return string(body), header.Filename, nil
		}
	}

	var body struct {
		Content  string `json:"content"`
		Filename string `json:"filename"`
	}
	if decodeErr := json.NewDecoder(io.LimitReader(r.Body, maxImportBytes)).Decode(&body); decodeErr != nil || body.Content == "" {
		return "", "", fmt.Errorf("missing file or content")
	}
	return body.Content, body.Filename, nil
}

// Export renders the selected skill version as a markdown blob (YAML
// frontmatter + instructions body) matching skillExportMd's contract: a
// text/markdown body with a Content-Disposition filename header the
// frontend parses to name the downloaded file.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	skillID := chi.URLParam(r, "skillID")
	versionID := chi.URLParam(r, "versionID")

	// #874: the {versionID} segment was already accepted by the route
	// (skill_export/{mode}/{projectID}/{skillID}/{versionID}) but never read
	// here — every export answered `base`'s content regardless. serializeSkillMarkdown
	// reads sk.Instructions/sk.Tags, which GetVersion sets to the requested
	// version.
	var sk Skill
	var err error
	if versionID != "" {
		sk, err = h.repo.GetVersion(r.Context(), projectID, skillID, versionID)
	} else {
		sk, err = h.repo.Get(r.Context(), projectID, skillID)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}

	content, err := serializeSkillMarkdown(sk)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to render skill markdown"))
		return
	}

	filename := sanitizeFilename(sk.Name) + ".md"
	w.Header().Set("Content-Type", "text/markdown")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func skillAuthor(r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0, false
	}
	return user.OwningUserID()
}

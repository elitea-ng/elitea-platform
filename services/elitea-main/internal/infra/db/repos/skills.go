package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type SkillsRepo struct {
	pool *pgxpool.Pool
}

func NewSkillsRepo(pool *pgxpool.Pool) *SkillsRepo {
	return &SkillsRepo{pool: pool}
}

// skillsListColumns is the SELECT list shared by List/Get: skill fields plus
// the base version and its aggregated tag names. sv is a LEFT JOIN so a
// skill created before this join existed (or with no base version yet)
// still returns a row, just with a NULL version id and empty tags.
const skillsSelectColumns = `
	sk.id, sk.name, COALESCE(sk.description, ''), sk.owner_id, sk.created_at,
	sv.id, COALESCE(sv.instructions, ''), sv.meta,
	COALESCE(array_agg(t.name ORDER BY t.name) FILTER (WHERE t.name IS NOT NULL), '{}')`

func skillsFromJoin(s string) string {
	return fmt.Sprintf(`FROM %s.skills sk
		LEFT JOIN %s.skill_versions sv ON sv.skill_id = sk.id AND sv.name = 'base'
		LEFT JOIN %s.skill_version_tag_association svta ON svta.version_id = sv.id
		LEFT JOIN %s.tags t ON t.id = svta.tag_id`, s, s, s, s)
}

func scanSkillRow(row pgx.Row, projectID string) (skills.Skill, error) {
	var sk skills.Skill
	var ownerID int
	var versionID *int
	var instructions string
	var meta map[string]any
	var tags []string
	if err := row.Scan(&sk.ID, &sk.Name, &sk.Description, &ownerID, &sk.CreatedAt, &versionID, &instructions, &meta, &tags); err != nil {
		return skills.Skill{}, err
	}
	sk.ProjectID = projectID
	sk.Type = "skill"
	sk.Instructions = instructions
	sk.Tags = tags
	if versionID != nil {
		// meta carries icon_meta. An empty map is omitted rather than sent as
		// `{}`, so a skill with no icon reads the same as it did before the
		// column was projected.
		if len(meta) == 0 {
			meta = nil
		}
		v := skills.SkillVersion{ID: strconv.Itoa(*versionID), Name: "base", Instructions: instructions, Tags: tags, Meta: meta}
		sk.Versions = []skills.SkillVersion{v}
		sk.VersionDetails = &v
	}
	return sk, nil
}

func (r *SkillsRepo) List(ctx context.Context, projectID string, params skills.ListParams) (skills.ListResponse, error) {
	s := schema(projectID)

	var args []any
	where := ""
	if params.Query != "" {
		where = ` WHERE (sk.name ILIKE $1 OR sk.description ILIKE $1)`
		args = append(args, "%"+params.Query+"%")
	}

	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.skills sk`, s) + where
	var total int
	if err := r.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return skills.ListResponse{Items: []skills.Skill{}, Total: 0, Page: params.Page, PageSize: params.PageSize}, nil
	}

	sortColumn := "sk.created_at"
	switch params.SortBy {
	case "name":
		sortColumn = "sk.name"
	}
	sortDir := "DESC"
	if strings.EqualFold(params.SortOrder, "asc") {
		sortDir = "ASC"
	}

	offset := (params.Page - 1) * params.PageSize
	limitIdx := len(args) + 1
	offsetIdx := len(args) + 2

	q := fmt.Sprintf(`SELECT %s %s`, skillsSelectColumns, skillsFromJoin(s)) + where +
		fmt.Sprintf(` GROUP BY sk.id, sv.id ORDER BY %s %s LIMIT $%d OFFSET $%d`, sortColumn, sortDir, limitIdx, offsetIdx)

	queryArgs := append(append([]any{}, args...), params.PageSize, offset)
	rows, err := r.pool.Query(ctx, q, queryArgs...)
	if err != nil {
		return skills.ListResponse{Items: []skills.Skill{}, Total: 0, Page: params.Page, PageSize: params.PageSize}, nil
	}
	defer rows.Close()

	var items []skills.Skill
	for rows.Next() {
		sk, err := scanSkillRow(rows, projectID)
		if err != nil {
			continue
		}
		items = append(items, sk)
	}
	if items == nil {
		items = []skills.Skill{}
	}

	totalPages := total / params.PageSize
	if total%params.PageSize > 0 {
		totalPages++
	}

	return skills.ListResponse{
		Items:      items,
		Total:      total,
		Page:       params.Page,
		PageSize:   params.PageSize,
		TotalPages: totalPages,
	}, nil
}

// NOTE(#395): `func (r *SkillsRepo) ListForApplicationVersion` stood here. It
// read entity_skill_mapping for one agent version and served
// GET /elitea_core/application_skills/{mode}/{projectID}/{appVersionID}, the
// PROTOTYPE fallback that #395 deleted.
//
// internal/api/v2/applicationskills owns that read now. Its repository runs
// the same join through a transaction-local tenant search_path and is covered
// by TestCurrentApplicationSkillsRoutePostgresContractAndTenantIsolation, so
// the version-scoping guarantee #367 asked for is still measured — on the
// route that answers.

// AttachSkill writes one entity_skill_mapping row.
//
// THE TRANSACTION DECISION. Every guard below is a read whose answer the INSERT
// depends on, so the guards and the INSERT run in ONE transaction. Outside one
// they are all time-of-check-to-time-of-use: a publish that lands between the
// status read and the INSERT attaches a skill to a published version, which is
// the state the guard exists to refuse, and `entity_skill_mapping.entity_version_id`
// carries no foreign key, so nothing at the database level catches it after the
// fact. Pylon makes the same choice — `_skill_session` owns one session and one
// commit per attach (legacy/plugins/elitea_core/utils/skill_utils.py:169-188).
//
// This is the opposite call from #414, and for a reason that is visible here.
// There the copy sat inside `embedSubAgentsRecursive`, a loop that continues
// past a failed sub-agent on purpose; one aborted statement inside a shared
// transaction would raise 25P02 on every later statement and take the sibling
// sub-agents with it. This function has no loop, no sibling and no
// partial-success channel. It performs one attach. All-or-nothing is the only
// state a caller can act on.
//
// The order of the guards is pylon's order, not a tidier one
// (skill_utils.py:1190-1233). The limit runs before the skill lookup, so a full
// agent version with a bad skill id answers 400 and not 404. Reordering would
// change the answer to a request the old app can send.
func (r *SkillsRepo) AttachSkill(
	ctx context.Context,
	projectID, skillID string,
	relation skills.SkillRelation,
) (skills.SkillAttachment, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return skills.SkillAttachment{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := guardEntityVersion(ctx, tx, s, relation, true); err != nil {
		return skills.SkillAttachment{}, err
	}

	var attached int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT COUNT(*) FROM %s.entity_skill_mapping
		WHERE entity_version_id = $1 AND entity_type = $2`, s),
		relation.EntityVersionID, relation.EntityType).Scan(&attached); err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: count: %w", err)
	}
	if attached >= skills.MaxSkillsPerEntityVersion {
		return skills.SkillAttachment{}, apierr.BadRequest(fmt.Sprintf(
			"Agent version %s already has %d skills attached. Maximum allowed is %d.",
			relation.EntityVersionID, attached, skills.MaxSkillsPerEntityVersion))
	}

	var skillName string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.skills WHERE id = $1`, s),
		skillID).Scan(&skillName)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.SkillAttachment{}, apierr.NotFound(fmt.Sprintf("Skill with id %s not found", skillID))
	}
	if err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: read skill: %w", err)
	}

	// The skill version must belong to THIS skill. The foreign key only says
	// the version exists; it does not say whose it is. Without the skill_id
	// predicate an attach could bind skill A's name to skill B's instructions,
	// because both readers take the instructions from skill_version_id and the
	// name from skill_id.
	var versionName string
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT name FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s),
		relation.SkillVersionID, skillID).Scan(&versionName)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.SkillAttachment{}, apierr.NotFound(fmt.Sprintf(
			"Skill version with id %s not found for skill %s", relation.SkillVersionID, skillID))
	}
	if err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: read skill version: %w", err)
	}

	// A duplicate attach is a 409, NOT an upsert and NOT a silent no-op.
	//
	// Pylon checks first and raises SkillAlreadyAttachedError
	// (skill_utils.py:1212-1218), and the old app depends on the refusal: its
	// version selector changes a skill's version by detaching and re-attaching,
	// with the comment "Backend errors on duplicate attach"
	// (apps/elitea-ui/src/[fsd]/features/skill/ui/SkillVersionSelector.jsx:54-65).
	// An ON CONFLICT DO UPDATE would make that first detach look pointless and
	// change a documented status code. The unique constraint stays as the
	// backstop for the rare race between this read and the INSERT.
	var existing int
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT 1 FROM %s.entity_skill_mapping
		WHERE entity_version_id = $1 AND entity_type = $2 AND skill_id = $3`, s),
		relation.EntityVersionID, relation.EntityType, skillID).Scan(&existing)
	if err == nil {
		return skills.SkillAttachment{}, apierr.Conflict(fmt.Sprintf(
			"Skill %s is already attached to agent version %s", skillID, relation.EntityVersionID))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: read mapping: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES ($1, $2, $3, $4)`, s),
		relation.EntityVersionID, relation.EntityType, skillID, relation.SkillVersionID); err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.SkillAttachment{}, fmt.Errorf("skills: attach: commit: %w", err)
	}

	return skills.SkillAttachment{
		SkillID:        atoiOrZero(skillID),
		SkillVersionID: atoiOrZero(relation.SkillVersionID),
		SkillName:      skillName,
		VersionName:    versionName,
	}, nil
}

// DetachSkill removes one entity_skill_mapping row.
//
// It runs in a transaction for the same reason AttachSkill does: the
// published-version guard is a read the DELETE depends on.
//
// A detach that matches no row is a 404, not a silent success. Pylon raises
// SkillNotAttachedError (skill_utils.py:1257-1258), and the old app reads the
// outcome: its version selector only re-attaches when the detach reports
// success, so a false success would drop the skill and report a version change
// that did not happen.
func (r *SkillsRepo) DetachSkill(
	ctx context.Context,
	projectID, skillID string,
	relation skills.SkillRelation,
) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("skills: detach: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := guardEntityVersion(ctx, tx, s, relation, false); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s.entity_skill_mapping
		WHERE entity_version_id = $1 AND entity_type = $2 AND skill_id = $3`, s),
		relation.EntityVersionID, relation.EntityType, skillID)
	if err != nil {
		return fmt.Errorf("skills: detach: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound(fmt.Sprintf(
			"Skill %s is not attached to agent version %s", skillID, relation.EntityVersionID))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("skills: detach: commit: %w", err)
	}
	return nil
}

// guardEntityVersion refuses a change to a version that must not change.
//
// A published or an embedded version is frozen: the publish path copies its
// skill rows into the public catalog (#405) and into every embedded sub-agent
// (#414), so a later attach or detach on the source would leave the two copies
// disagreeing with no way to tell which one a consumer holds. Pylon raises
// AgentVersionNotUpdatableError on both directions, at 409
// (legacy/plugins/elitea_core/utils/skill_utils.py:1179-1183 and 1245-1249).
//
// `requireVersion` is true for attach only, and this is a deliberate departure
// from pylon. Pylon writes `if agent_version and ...`, so an unknown version id
// falls through and the attach answers 201 over a row that no read can ever
// reach: `entity_version_id` has no foreign key, so the row is an orphan that
// nothing cleans up. Attach therefore refuses an unknown version. Detach keeps
// pylon's fall-through, because removing an orphan left by an earlier write is
// the one case where an absent version is still a valid request.
func guardEntityVersion(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	relation skills.SkillRelation,
	requireVersion bool,
) error {
	var status string
	err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(status, '') FROM %s.application_versions WHERE id = $1`, schema),
		relation.EntityVersionID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		if requireVersion {
			return apierr.NotFound(fmt.Sprintf(
				"Agent version %s not found", relation.EntityVersionID))
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("skills: read agent version: %w", err)
	}
	if status == "published" || status == "embedded" {
		return apierr.Conflict(fmt.Sprintf(
			"Agent version %s is %s and can not be updated", relation.EntityVersionID, status))
	}
	return nil
}

// atoiOrZero converts an id the handler already proved is a positive 32-bit
// integer. The error cannot happen, and a zero would be visible in the response.
func atoiOrZero(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// Get returns the skill with EVERY skill_versions row (#874) —
// Versions/VersionDetails are no longer just `base`. Instructions/Tags/
// VersionDetails keep pointing at `base`, exactly as before: the
// unversioned GET/PUT/DELETE always work on `base`, and this is the read
// half of that contract. List's own per-row projection (skillsSelectColumns/
// skillsFromJoin above) deliberately stays base-only — a paginated list of
// skills has no use for every skill's full version history, only a single
// skill's detail view does.
func (r *SkillsRepo) Get(ctx context.Context, projectID, skillID string) (skills.Skill, error) {
	return r.getSkillWithVersions(ctx, projectID, skillID, "")
}

// GetVersion returns the skill with VersionDetails/Instructions/Tags pointed
// at versionID instead of `base`. NotFound when versionID does not belong to
// skillID — getSkillWithVersions itself falls back to `base` on a miss
// (the shape every OTHER caller of it wants), so this is the one caller that
// has to notice the miss and turn it into an error.
func (r *SkillsRepo) GetVersion(ctx context.Context, projectID, skillID, versionID string) (skills.Skill, error) {
	sk, err := r.getSkillWithVersions(ctx, projectID, skillID, versionID)
	if err != nil {
		return skills.Skill{}, err
	}
	if sk.VersionDetails == nil || sk.VersionDetails.ID != versionID {
		return skills.Skill{}, apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}
	return sk, nil
}

// getSkillWithVersions is the shared builder behind Get, GetVersion and
// every version-mutating method's response (#874): one skills row plus
// EVERY skill_versions row for it — not just `base`, the way
// scanSkillRow/skillsFromJoin/List still read. selectedVersionID, when
// non-empty and it names a version of this skill, points
// VersionDetails/Instructions/Tags at that version instead of `base`.
//
// This function does not error when selectedVersionID names no version of
// the skill — it silently falls back to `base` instead, the shape every
// caller except GetVersion wants (an unversioned write with a stray
// selectedVersionID should still succeed and describe the skill it actually
// changed). GetVersion is the one caller that must turn a miss into 404, and
// it does that itself by comparing VersionDetails.ID against what it asked
// for.
func (r *SkillsRepo) getSkillWithVersions(ctx context.Context, projectID, skillID, selectedVersionID string) (skills.Skill, error) {
	s := schema(projectID)

	var sk skills.Skill
	var meta map[string]any
	err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT id, name, COALESCE(description, ''), created_at, meta FROM %s.skills WHERE id = $1`, s),
		skillID).Scan(&sk.ID, &sk.Name, &sk.Description, &sk.CreatedAt, &meta)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skills.Skill{}, apierr.NotFound("skill not found")
		}
		return skills.Skill{}, fmt.Errorf("skills: get: %w", err)
	}
	sk.ProjectID = projectID
	sk.Type = "skill"
	if defaultID, ok := meta["default_version_id"].(string); ok {
		sk.DefaultVersionID = defaultID
	}

	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT sv.id, sv.name, COALESCE(sv.instructions, ''), sv.meta, sv.status, sv.created_at, sv.parent_version_id,
			COALESCE(array_agg(t.name ORDER BY t.name) FILTER (WHERE t.name IS NOT NULL), '{}')
		FROM %s.skill_versions sv
		LEFT JOIN %s.skill_version_tag_association svta ON svta.version_id = sv.id
		LEFT JOIN %s.tags t ON t.id = svta.tag_id
		WHERE sv.skill_id = $1
		GROUP BY sv.id
		ORDER BY (sv.name = 'base') DESC, sv.created_at ASC, sv.id ASC`, s, s, s), skillID)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: get versions: %w", err)
	}
	defer rows.Close()

	var versions []skills.SkillVersion
	for rows.Next() {
		v, err := scanSkillVersionRow(rows)
		if err != nil {
			return skills.Skill{}, fmt.Errorf("skills: scan version: %w", err)
		}
		if sk.DefaultVersionID != "" && v.ID == sk.DefaultVersionID {
			v.IsDefault = true
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: get versions: %w", err)
	}
	sk.Versions = versions

	var current *skills.SkillVersion
	if selectedVersionID != "" {
		for i := range versions {
			if versions[i].ID == selectedVersionID {
				current = &versions[i]
				break
			}
		}
	}
	if current == nil {
		for i := range versions {
			if versions[i].Name == "base" {
				current = &versions[i]
				break
			}
		}
	}
	if current == nil && len(versions) > 0 {
		current = &versions[0]
	}
	if current != nil {
		sk.Instructions = current.Instructions
		sk.Tags = current.Tags
		picked := *current
		sk.VersionDetails = &picked
	}
	return sk, nil
}

func scanSkillVersionRow(row pgx.Row) (skills.SkillVersion, error) {
	var v skills.SkillVersion
	var id int
	var meta map[string]any
	var parentID *int
	var tags []string
	if err := row.Scan(&id, &v.Name, &v.Instructions, &meta, &v.Status, &v.CreatedAt, &parentID, &tags); err != nil {
		return skills.SkillVersion{}, err
	}
	v.ID = strconv.Itoa(id)
	if len(meta) > 0 {
		v.Meta = meta
	}
	if parentID != nil {
		v.ParentVersionID = strconv.Itoa(*parentID)
	}
	v.Tags = tags
	return v, nil
}

func (r *SkillsRepo) GetByName(ctx context.Context, projectID, name string) (skills.Skill, bool, error) {
	s := schema(projectID)
	var id string
	err := r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.skills WHERE name = $1 ORDER BY id LIMIT 1`, s), name).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skills.Skill{}, false, nil
		}
		return skills.Skill{}, false, fmt.Errorf("skills: get by name: %w", err)
	}
	sk, err := r.Get(ctx, projectID, id)
	if err != nil {
		return skills.Skill{}, false, err
	}
	return sk, true, nil
}

func (r *SkillsRepo) Create(ctx context.Context, projectID string, skill skills.Skill) (skills.Skill, error) {
	s := schema(projectID)
	// owner_id is the OWNING PROJECT, not the creating user — see
	// createSkillSQL. A project id that names no schema stops here with a 400
	// rather than reaching a statement built from a fail-closed sentinel.
	ownerID, err := tenantschema.OwnerID(projectID)
	if err != nil {
		return skills.Skill{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sk skills.Skill
	err = tx.QueryRow(ctx, createSkillSQL(s),
		skill.Name, skill.Description, ownerID.Int64()).Scan(&sk.ID, &sk.Name, &sk.Description, &sk.CreatedAt)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: create: %w", err)
	}

	version, err := upsertBaseSkillVersion(ctx, tx, s, sk.ID, skill.Instructions, skill.Tags)
	if err != nil {
		return skills.Skill{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: create: commit: %w", err)
	}

	sk.ProjectID = projectID
	sk.Type = "skill"
	sk.Instructions = version.Instructions
	sk.Tags = version.Tags
	sk.Versions = []skills.SkillVersion{version}
	sk.VersionDetails = &version
	return sk, nil
}

func (r *SkillsRepo) Update(ctx context.Context, projectID, skillID string, skill skills.Skill) (skills.Skill, error) {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sk skills.Skill
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.skills SET name = $1, description = $2
		WHERE id = $3
		RETURNING id, name, COALESCE(description, ''), created_at`, s),
		skill.Name, skill.Description, skillID).Scan(&sk.ID, &sk.Name, &sk.Description, &sk.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skills.Skill{}, apierr.NotFound("skill not found")
		}
		return skills.Skill{}, fmt.Errorf("skills: update: %w", err)
	}

	version, err := upsertBaseSkillVersion(ctx, tx, s, skillID, skill.Instructions, skill.Tags)
	if err != nil {
		return skills.Skill{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update: commit: %w", err)
	}

	sk.ProjectID = projectID
	sk.Type = "skill"
	sk.Instructions = version.Instructions
	sk.Tags = version.Tags
	sk.Versions = []skills.SkillVersion{version}
	sk.VersionDetails = &version
	return sk, nil
}

// upsertBaseSkillVersion upserts the skill's single "base" skill_versions row
// (unique on (skill_id, name), see 001_initial.sql) and replaces its tag
// associations by delete-then-reinsert — mirrors applications.go's own
// delete-cascade pattern for the equivalent application_version_tag_association
// table. Tags are upserted by name (tags.name is UNIQUE) so repeated tag
// names across skills share one tags row.
func upsertBaseSkillVersion(ctx context.Context, tx pgx.Tx, schema, skillID, instructions string, tags []string) (skills.SkillVersion, error) {
	v := skills.SkillVersion{Name: "base", Instructions: instructions}

	var versionID int
	err := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.skill_versions (skill_id, name, instructions, author_id)
		VALUES ($1, 'base', $2, 1)
		ON CONFLICT (skill_id, name) DO UPDATE SET instructions = EXCLUDED.instructions
		RETURNING id`, schema), skillID, instructions).Scan(&versionID)
	if err != nil {
		return skills.SkillVersion{}, fmt.Errorf("skills: upsert version: %w", err)
	}
	v.ID = strconv.Itoa(versionID)

	replaced, err := replaceVersionTags(ctx, tx, schema, versionID, tags)
	if err != nil {
		return skills.SkillVersion{}, err
	}
	v.Tags = replaced

	return v, nil
}

// replaceVersionTags replaces versionID's tag associations by
// delete-then-reinsert (#874's shared version of the loop
// upsertBaseSkillVersion used to run inline) and returns the tag names
// actually attached — deduplicated, in call order, and always non-nil so a
// version with no tags serializes as `[]`, never `null`.
func replaceVersionTags(ctx context.Context, tx pgx.Tx, schema string, versionID int, tags []string) ([]string, error) {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.skill_version_tag_association WHERE version_id = $1`, schema), versionID); err != nil {
		return nil, fmt.Errorf("skills: clear tags: %w", err)
	}

	result := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, tagName := range tags {
		tagName = strings.TrimSpace(tagName)
		if tagName == "" || seen[tagName] {
			continue
		}
		seen[tagName] = true

		var tagID int
		err := tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, schema), tagName).Scan(&tagID)
		if err != nil {
			return nil, fmt.Errorf("skills: upsert tag %q: %w", tagName, err)
		}

		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.skill_version_tag_association (version_id, tag_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, schema), versionID, tagID); err != nil {
			return nil, fmt.Errorf("skills: link tag %q: %w", tagName, err)
		}
		result = append(result, tagName)
	}

	return result, nil
}

// sourceVersionTags reads the tag names attached to one skill_versions row,
// for CreateVersion's/RestoreVersion's clone path.
func sourceVersionTags(ctx context.Context, tx pgx.Tx, schema string, versionID any) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT t.name FROM %s.skill_version_tag_association svta
		JOIN %s.tags t ON t.id = svta.tag_id
		WHERE svta.version_id = $1 ORDER BY t.name`, schema, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("skills: read source tags: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("skills: scan source tag: %w", err)
		}
		tags = append(tags, name)
	}
	return tags, rows.Err()
}

// CreateVersion adds one NAMED skill_versions row ("Save As Version", #874).
// When input.Instructions is empty it clones input.SourceVersionID's content
// (default `base`) instead of creating an empty version — the fallback
// entity-versioning.mdx documents for agents/pipelines' own "Save As
// Version", even though the frontend's current createSkillVersion() call
// always sends full content and never exercises this branch.
func (r *SkillsRepo) CreateVersion(ctx context.Context, projectID, skillID string, input skills.VersionCreateInput) (skills.Skill, error) {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: create version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Confirmed up front: the unique-violation path below cannot tell "no
	// such skill" apart from "duplicate version name" — both raise 23505 on
	// FOREIGN KEY vs UNIQUE respectively, but a missing skill_id fails the
	// FK, and reporting a name conflict for a skill that does not exist
	// would be the wrong 409.
	var exists int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT 1 FROM %s.skills WHERE id = $1`, s), skillID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skills.Skill{}, apierr.NotFound("skill not found")
		}
		return skills.Skill{}, fmt.Errorf("skills: create version: read skill: %w", err)
	}

	instructions := input.Instructions
	tags := input.Tags
	var parentID *int
	if instructions == "" {
		var srcID int
		var q string
		var args []any
		if input.SourceVersionID != "" {
			q = fmt.Sprintf(`SELECT id, COALESCE(instructions, '') FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s)
			args = []any{input.SourceVersionID, skillID}
		} else {
			q = fmt.Sprintf(`SELECT id, COALESCE(instructions, '') FROM %s.skill_versions WHERE skill_id = $1 AND name = 'base'`, s)
			args = []any{skillID}
		}
		if err := tx.QueryRow(ctx, q, args...).Scan(&srcID, &instructions); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return skills.Skill{}, apierr.NotFound("source skill version not found")
			}
			return skills.Skill{}, fmt.Errorf("skills: create version: read source: %w", err)
		}
		if len(tags) == 0 {
			cloned, err := sourceVersionTags(ctx, tx, s, srcID)
			if err != nil {
				return skills.Skill{}, err
			}
			tags = cloned
		}
		parentID = &srcID
	} else if input.SourceVersionID != "" {
		if parsed, err := strconv.Atoi(input.SourceVersionID); err == nil {
			parentID = &parsed
		}
	}

	var newID int
	insertErr := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.skill_versions (skill_id, name, instructions, author_id, parent_version_id)
		VALUES ($1, $2, $3, 1, $4)
		RETURNING id`, s), skillID, input.Name, instructions, parentID).Scan(&newID)
	if insertErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(insertErr, &pgErr) && pgErr.Code == "23505" {
			return skills.Skill{}, apierr.Conflict(fmt.Sprintf("a version named %q already exists", input.Name))
		}
		return skills.Skill{}, fmt.Errorf("skills: create version: insert: %w", insertErr)
	}

	if _, err := replaceVersionTags(ctx, tx, s, newID, tags); err != nil {
		return skills.Skill{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: create version: commit: %w", err)
	}
	return r.getSkillWithVersions(ctx, projectID, skillID, strconv.Itoa(newID))
}

// UpdateVersion edits ONE named version's instructions/tags, plus the
// skill's own name/description (shared across every version, exactly as
// Update writes them). Refuses a published version with Conflict, mirroring
// application_versions' "Unpublish first" guard
// (internal/api/v2/applications/handler.go).
func (r *SkillsRepo) UpdateVersion(ctx context.Context, projectID, skillID, versionID string, skill skills.Skill) (skills.Skill, error) {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.skills SET name = $1, description = $2 WHERE id = $3`, s),
		skill.Name, skill.Description, skillID)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update version: update skill: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return skills.Skill{}, apierr.NotFound("skill not found")
	}

	var status string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT status FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s),
		versionID, skillID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Skill{}, apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update version: read version: %w", err)
	}
	if status == "published" {
		return skills.Skill{}, apierr.Conflict("Unpublish first. Cannot update a published version.")
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.skill_versions SET instructions = $1 WHERE id = $2`, s),
		skill.Instructions, versionID); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update version: update instructions: %w", err)
	}
	versionIntID, _ := strconv.Atoi(versionID)
	if _, err := replaceVersionTags(ctx, tx, s, versionIntID, skill.Tags); err != nil {
		return skills.Skill{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: update version: commit: %w", err)
	}
	return r.getSkillWithVersions(ctx, projectID, skillID, versionID)
}

// DeleteVersion removes one named skill_versions row. Refuses `base` and the
// current default version with BadRequest — deleting either would leave the
// unversioned GET/PUT/DELETE contract or a fresh attachment's proposed
// version pointing at nothing — and a published version with Conflict.
func (r *SkillsRepo) DeleteVersion(ctx context.Context, projectID, skillID, versionID string) error {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("skills: delete version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name, status string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT name, status FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s),
		versionID, skillID).Scan(&name, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}
	if err != nil {
		return fmt.Errorf("skills: delete version: read version: %w", err)
	}
	if name == "base" {
		return apierr.BadRequest(`cannot delete the "base" version`)
	}
	if status == "published" {
		return apierr.Conflict("Unpublish first. Cannot delete a published version.")
	}

	var meta map[string]any
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT meta FROM %s.skills WHERE id = $1`, s), skillID).Scan(&meta); err != nil {
		return fmt.Errorf("skills: delete version: read skill: %w", err)
	}
	if defaultID, ok := meta["default_version_id"].(string); ok && defaultID == versionID {
		return apierr.BadRequest("cannot delete the default version; set a different default first")
	}

	ct, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s), versionID, skillID)
	if err != nil {
		return fmt.Errorf("skills: delete version: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("skills: delete version: commit: %w", err)
	}
	return nil
}

// RestoreVersion is the rollback (#874's headline gap): it copies
// versionID's instructions/tags back onto `base` and records the lineage
// (parent_version_id), then returns the skill with `base` as VersionDetails.
//
// Unlike agents' "Set as default" — which repoints a pointer and leaves
// every version's content untouched — skills have no distinguished
// "currently active" row to repoint: entity_skill_mapping pins a
// skill_version_id at ATTACH time (skills.go's AttachSkill), and the
// unversioned GET/PUT/DELETE always read/write `base`. Restoring a version
// therefore means overwriting the one row those two things actually use,
// not moving a flag.
func (r *SkillsRepo) RestoreVersion(ctx context.Context, projectID, skillID, versionID string) (skills.Skill, error) {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: restore version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var instructions string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(instructions, '') FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s),
		versionID, skillID).Scan(&instructions)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Skill{}, apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: restore version: read source: %w", err)
	}
	tags, err := sourceVersionTags(ctx, tx, s, versionID)
	if err != nil {
		return skills.Skill{}, err
	}

	var baseID int
	var baseStatus string
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id, status FROM %s.skill_versions WHERE skill_id = $1 AND name = 'base'`, s),
		skillID).Scan(&baseID, &baseStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Skill{}, apierr.NotFound("skill not found")
	}
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: restore version: read base: %w", err)
	}
	if baseStatus == "published" {
		return skills.Skill{}, apierr.Conflict("Unpublish first. Cannot restore over a published base version.")
	}

	sourceID, _ := strconv.Atoi(versionID)
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.skill_versions SET instructions = $1, parent_version_id = $2 WHERE id = $3`, s),
		instructions, sourceID, baseID); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: restore version: update base: %w", err)
	}
	if _, err := replaceVersionTags(ctx, tx, s, baseID, tags); err != nil {
		return skills.Skill{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: restore version: commit: %w", err)
	}
	return r.getSkillWithVersions(ctx, projectID, skillID, strconv.Itoa(baseID))
}

// SetDefaultVersion writes skills.meta.default_version_id, mirroring
// applications.meta.default_version_id (repos/applications.go). It does not
// change which version any EXISTING agent attachment resolves at chat time —
// entity_skill_mapping.skill_version_id is fixed at attach time and this
// column plays no part in agent_chat.sql's turn-resolution join — only what
// a NEW attachment's picker proposes.
func (r *SkillsRepo) SetDefaultVersion(ctx context.Context, projectID, skillID, versionID string) (skills.Skill, error) {
	s := schema(projectID)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: set default version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists int
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT 1 FROM %s.skill_versions WHERE id = $1 AND skill_id = $2`, s),
		versionID, skillID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Skill{}, apierr.NotFound(fmt.Sprintf("skill version %s not found for skill %s", versionID, skillID))
	}
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: set default version: read version: %w", err)
	}

	ct, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.skills SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('default_version_id', $1::text)
		WHERE id = $2`, s), versionID, skillID)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("skills: set default version: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return skills.Skill{}, apierr.NotFound("skill not found")
	}

	if err := tx.Commit(ctx); err != nil {
		return skills.Skill{}, fmt.Errorf("skills: set default version: commit: %w", err)
	}
	return r.getSkillWithVersions(ctx, projectID, skillID, "")
}

func (r *SkillsRepo) Delete(ctx context.Context, projectID, skillID string) error {
	s := schema(projectID)

	// Guard: a skill with a published version cannot be deleted (#249).
	//
	// The cascade below would take the source rows with it while the copy in
	// the public catalog survived — and unpublishing is keyed off the source
	// skill, so the author would be left with an entry they can see in the
	// catalog and no longer have any way to retract. Same guard, same reason
	// and same wording as applications
	// (internal/api/v2/applications/handler.go:669-681).
	var publishedCount int
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.skill_versions WHERE skill_id = $1 AND status = 'published'`, s),
		skillID).Scan(&publishedCount); err != nil {
		return fmt.Errorf("skills: delete: check published versions: %w", err)
	}
	if publishedCount > 0 {
		return apierr.BadRequest("Unpublish first. Cannot delete skill with published versions.")
	}

	// skill_versions and skill_version_tag_association both cascade on
	// delete (001_initial.sql), so no manual child cleanup is needed here
	// (unlike applications.go, whose equivalent tables lack ON DELETE CASCADE).
	q := fmt.Sprintf(`DELETE FROM %s.skills WHERE id = $1`, s)
	ct, err := r.pool.Exec(ctx, q, skillID)
	if err != nil {
		return fmt.Errorf("skills: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("skill not found")
	}
	return nil
}

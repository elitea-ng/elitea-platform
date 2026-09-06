package eliteacore

// The skill half of the import and the fork — issue #611.
//
// # What was wrong
//
// `ExportImportPost` read three keys off a version entry: `tools`, `variables`
// and `tags`. `Fork` copied `applications`, `application_versions`,
// `application_variables` and the tag association. Neither read a skill, and
// neither wrote a row of `entity_skill_mapping`. So an agent that a user gave
// skills to lost every one of them the moment it was exported and imported
// again, or forked, and no message said so anywhere.
//
// # The document
//
// A skill is an ENTITY of its own, beside `agents` and `toolkits`. The file
// carries its whole content, and the import creates it in the destination
// project: a `skill_id` from the project the file came from names no row in the
// project it is imported into, so nothing else can work. A version entry then
// carries `skills`, an array of references that name the skill by
// `import_uuid` and the version by `version_name`.
//
// This is pylon's shape, unchanged, because the same files move between the two
// implementations and the old app builds it as well:
//
//   - legacy/plugins/elitea_core/utils/export_import.py, `_export_skills_main`
//     writes it,
//   - legacy/plugins/elitea_core/rpc/import_wizzard.py, `_attach_imported_skills`
//     reads it,
//   - apps/elitea-ui .../importWizardParser.helpers.js,
//     `buildSkillsFromFrontmatter` rebuilds both halves out of a markdown file
//     and `prepareImportWizardData` turns every top-level array into entries
//     that carry `entity: "<the array's key>"`.
//
// That last one is why this file also FIXES a second fault. The wizard already
// sent `entity: "skills"` entries — from any markdown file that carries a
// skills block — and this service had no branch for them. They fell to the
// `else` that treats an entry as an agent, so importing such a file created a
// phantom AGENT named after each skill.
//
// # Why an imported skill always gets a `base` version
//
// The export ships only the versions the attachments name, so an agent pinned
// to a version called `reviewed` exports a skill whose whole version list is
// `[reviewed]`. Written to the destination as it stands, that skill is
// INVISIBLE to every skills read this service has: `skillsFromJoin`
// (internal/infra/db/repos/skills.go:34-39) LEFT JOINs `sv.name = 'base'`, and
// `scanSkillRow` (:42-60) leaves the instructions, the versions and the version
// details empty when that join finds nothing. The skill list, the skill page and
// the agent's own Skills panel would each show a name with no content, and a
// user who saved from that empty editor would make `upsertBaseSkillVersion`
// write the empty body over it for good. Chat would meanwhile run correctly,
// because it reads `skill_version_id` and not the name
// (internal/db/queries/agent_chat.sql:126-132) — so nothing would report it.
//
// The reach is not theoretical and does not need pylon. `skillBlocks`
// (export_markdown.go) writes `version: <name>` into the markdown this service
// hands the user to edit, and `buildSkillsFromFrontmatter`
// (apps/elitea-ui .../importWizardParser.helpers.js:142) reads that field back
// as `name: block.version || 'base'`. Export an agent with a pinned skill and
// import the file again, and the loop closes inside this product.
//
// `ensureBaseSkillVersion` is pylon's answer, unchanged
// (utils/skill_export_import.py:43-59, issue #5469). It is ADDITIVE: the named
// versions all survive, so a reference that pins one still resolves to it, and a
// `base` clone of the first version is prepended so the reads have their row.
// Every other writer of this table in this service already holds that invariant
// — `skillpublish/export_fork.go:84` emits the copy as the target's single
// `base`, `skillpublish/attach.go:201` does the same, and
// `upsertBaseSkillVersion` writes no other name.
//
// # One pylon behaviour this deliberately leaves out
//
//   - `_find_forked_skill` (utils/skill_utils.py) reuses a skill this project
//     already forked from the same source, keyed on the lineage in the version
//     meta. Without it, forking one agent twice makes two copies of its skill —
//     which is what forking one agent twice already does to the agent.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// importedSkill is one skill that the import or the fork created, addressed the
// way a version reference addresses it.
type importedSkill struct {
	id int
	// versions maps a version NAME to the new `skill_versions.id`. The name is
	// the key because it is what a reference carries: the ids in the file belong
	// to the project the file came from.
	versions map[string]int
	// firstVersion is the name of the first version the document listed. It is
	// the last fallback for a reference that names no version and no `base`.
	firstVersion string
}

// versionID resolves the version one reference names.
//
// The order is `_resolve_and_attach_skill`'s (rpc/import_wizzard.py:245-252):
// the named version, then `base`, then whatever the skill's first version is. A
// reference that names no version reaches this with an empty name and takes the
// second branch, which is the case an attachment whose `skill_version_id` is
// NULL exports as. `ensureBaseSkillVersion` makes that branch always resolve.
//
// The fallbacks matter because they are the difference between a skill that
// runs and a skill in name only: the chat read LEFT JOINs `skill_version_id`
// for the instructions (internal/db/queries/agent_chat.sql:126-132), so an
// attachment with no version gives the model an empty body.
func (s importedSkill) versionID(name string) (int, bool) {
	if name != "" {
		if versionID, found := s.versions[name]; found {
			return versionID, true
		}
	}
	if versionID, found := s.versions["base"]; found {
		return versionID, true
	}
	if versionID, found := s.versions[s.firstVersion]; found {
		return versionID, true
	}
	return 0, false
}

// importSkill writes one skill entity and its versions into one tenant schema.
//
// `ownerID` is the DESTINATION PROJECT and not a user: `p_<id>.skills.owner_id`
// is the owning project, which issue #533 measured and
// internal/infra/db/repos/skills_owner.go records. `authorID` is the caller.
//
// The two parameters carry different types for that reason. They were both
// `int` and adjacent in this list, so a call that swapped them compiled, ran,
// and wrote a user id into the project column. ownership.ProjectID and
// ownership.UserID make that call a compile error.
func (h *Handler) importSkill(
	ctx context.Context, schema string,
	ownerID ownership.ProjectID, authorID ownership.UserID,
	entry map[string]any,
) (importedSkill, error) {
	name, _ := entry["name"].(string)
	if name == "" {
		return importedSkill{}, fmt.Errorf("a skill needs a name")
	}
	description, _ := entry["description"].(string)
	if description == "" {
		// `skills.description` is NOT NULL (001_initial.sql:370-381), and the
		// legacy writes `description or name` into it (utils/skill_utils.py,
		// import_skill). An empty string would satisfy the column and give the
		// skill list a row with no text at all.
		description = name
	}
	importUUID, _ := entry["import_uuid"].(string)
	if importUUID == "" {
		// The key that makes this import repeatable, and the key every version
		// reference names. Without it the caller cannot link the skill to any
		// agent, so the entity is refused rather than written and then reported
		// as a success that nothing can use.
		return importedSkill{}, fmt.Errorf("skill %q needs an import_uuid", name)
	}
	meta, err := importedSkillMeta(entry, importUUID)
	if err != nil {
		return importedSkill{}, err
	}

	versions, _ := entry["versions"].([]any)
	if len(versions) == 0 {
		// The legacy raises here as well: a skill with no version has no
		// instructions, so nothing an agent attaches to it can run.
		return importedSkill{}, fmt.Errorf("skill %q carries no version to import", name)
	}
	versions = ensureBaseSkillVersion(versions)

	// THE IMPORT IS REPEATABLE.
	//
	// A plain INSERT here made every run write a new skill. That is wrong for the
	// action this wizard invites most: a partly failed import answers 207, the
	// wizard paints the failed entity red, and the user sends the SAME file
	// again. The skills that succeeded the first time were written again, so the
	// project collected one copy of every skill per attempt, each orphaned from
	// the agent whose import had failed.
	//
	// The source identity is the document's `import_uuid`, recorded in
	// `meta.import_uuid` on the row this writes. A second run of the same
	// document finds that row and reuses it, and the version INSERT below
	// upserts on `_skill_version_name_uc`, so a retry converges on the state the
	// first run was trying to reach rather than duplicating it.
	//
	// `owner_id` is in the predicate because it is the OWNING PROJECT (#533):
	// two projects that import one file each get their own skill, and neither
	// can reach the other's.
	//
	// This is not pylon's `_find_forked_skill`, which matches the lineage a FORK
	// writes into the version meta and so cannot see a plain import at all. It is
	// the same idea, keyed on the one identifier every import document carries.
	var skillID int
	err = h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id FROM %s.skills
		WHERE owner_id = $1 AND meta ->> 'import_uuid' = $2
		ORDER BY id LIMIT 1`, schema), ownerID.Int64(), importUUID).Scan(&skillID)
	switch {
	case err == nil:
		// Reused. The name and the description are refreshed, because the file
		// is the authority on what the skill is called and a retry can follow an
		// edit to it.
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			UPDATE %s.skills SET name = $2, description = $3 WHERE id = $1`, schema),
			skillID, name, description); err != nil {
			return importedSkill{}, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		metaJSON, encodeErr := importedJSONEncode("meta", meta)
		if encodeErr != nil {
			return importedSkill{}, encodeErr
		}
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.skills (name, description, owner_id, author_id, meta)
			VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`, schema),
			name, description, ownerID.Int64(), authorID.Int64(), metaJSON).Scan(&skillID); err != nil {
			return importedSkill{}, err
		}
	default:
		return importedSkill{}, err
	}

	imported := importedSkill{id: skillID, versions: map[string]int{}}
	for _, raw := range versions {
		version, isMap := raw.(map[string]any)
		if !isMap {
			return imported, fmt.Errorf("a version of skill %q is not a JSON object", name)
		}
		versionName, _ := version["name"].(string)
		if versionName == "" {
			versionName = "base"
		}
		instructions, _ := version["instructions"].(string)
		versionMetaJSON, err := importedJSONObject(version, "meta")
		if err != nil {
			return imported, fmt.Errorf("version %q of skill %q: %w", versionName, name, err)
		}

		var versionID int
		// `draft`, always, and never the status the file carries. `published`
		// means the project holds a twin of this version in the public project,
		// with the `shared_id` bookkeeping the publish route writes. An import
		// creates no such twin, and the delete guard refuses a skill that has a
		// published version (internal/infra/db/repos/skills.go:585-604) while
		// unpublishing is keyed off the twin. A copied `published` would
		// therefore give the destination project a skill it can neither retract
		// nor delete. `_skill_version_name_uc` is unique on (skill_id, name)
		// (001_initial.sql:395-406): the ON CONFLICT is there so that a file
		// carrying two versions of one name costs the duplicate and not the
		// whole skill, since a reference cannot tell those two apart anyway.
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.skill_versions (skill_id, name, instructions, author_id, status, meta)
			VALUES ($1, $2, $3, $4, 'draft', $5::jsonb)
			ON CONFLICT ON CONSTRAINT _skill_version_name_uc
			DO UPDATE SET instructions = EXCLUDED.instructions
			RETURNING id`, schema),
			skillID, versionName, instructions, authorID, versionMetaJSON).Scan(&versionID); err != nil {
			return imported, fmt.Errorf("version %q of skill %q: %w", versionName, name, err)
		}
		if imported.firstVersion == "" {
			imported.firstVersion = versionName
		}
		imported.versions[versionName] = versionID

		if err := h.importSkillVersionTags(ctx, schema, versionID, version["tags"]); err != nil {
			return imported, fmt.Errorf("version %q of skill %q: %w", versionName, name, err)
		}
	}
	return imported, nil
}

// importedSkillMeta reads the document's `meta` and records the source identity
// in it, so a second run of one document finds the row the first run wrote.
//
// The key goes in the skill's own meta rather than in a column of its own,
// because a jsonb key needs no migration. It is the value `importSkill` reads
// back with `meta ->> 'import_uuid'`.
func importedSkillMeta(entry map[string]any, importUUID string) (map[string]any, error) {
	raw, present := entry["meta"]
	if !present || raw == nil {
		return map[string]any{"import_uuid": importUUID}, nil
	}
	stored, isMap := raw.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("meta must be a JSON object")
	}
	meta := make(map[string]any, len(stored)+1)
	for key, value := range stored {
		meta[key] = value
	}
	meta["import_uuid"] = importUUID
	return meta, nil
}

// ensureBaseSkillVersion prepends a `base` clone when the imported versions
// carry none — pylon's `ensure_base_version`
// (utils/skill_export_import.py:43-59), for the reason the file header gives.
//
// ADDITIVE, and not a rename: the named versions all survive, so a reference
// that pins `reviewed` still finds `reviewed`. The clone copies the first
// version whole and changes only its name, so the base carries the same
// instructions, meta and tags. A version entry with no name at all is already
// written as `base` by the caller, so it counts as one here.
func ensureBaseSkillVersion(versions []any) []any {
	for _, raw := range versions {
		version, isMap := raw.(map[string]any)
		if !isMap {
			// Not a version this function can read. The caller reports it, and
			// it must not be treated as a missing base.
			continue
		}
		if versionName, _ := version["name"].(string); versionName == "" || versionName == "base" {
			return versions
		}
	}
	first, isMap := versions[0].(map[string]any)
	if !isMap {
		return versions
	}
	base := make(map[string]any, len(first))
	for key, value := range first {
		base[key] = value
	}
	base["name"] = "base"
	return append([]any{base}, versions...)
}

// importSkillVersionTags writes the tag associations of one imported skill
// version.
//
// It accepts a tag as an object with a `name`, which is what the export writes,
// and as a plain string, which is what a hand-written markdown file carries —
// the two shapes `_normalize_tags` accepts
// (utils/skill_export_import.py:62-71).
func (h *Handler) importSkillVersionTags(ctx context.Context, schema string, versionID int, raw any) error {
	tags, _ := raw.([]any)
	for _, entry := range tags {
		var tagName string
		switch typed := entry.(type) {
		case string:
			tagName = typed
		case map[string]any:
			tagName, _ = typed["name"].(string)
		}
		if tagName == "" {
			continue
		}
		var tagID int
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, schema), tagName).Scan(&tagID); err != nil {
			return fmt.Errorf("tag %q: %w", tagName, err)
		}
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.skill_version_tag_association (version_id, tag_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, schema), versionID, tagID); err != nil {
			return fmt.Errorf("tag %q: %w", tagName, err)
		}
	}
	return nil
}

// attachImportedSkills writes the `entity_skill_mapping` rows of one imported
// or forked agent version, and returns one message for each reference it could
// not attach.
//
// Nothing here is silent. That is the whole point of the issue: the three paths
// that lost these rows lost them without a word, and a caller who reads a 201
// has no way to learn that the agent came back with fewer skills than the file
// carries. Each message goes into the response the same way a failed toolkit
// link does (#420), so the wizard marks the entity and the user sees it.
func (h *Handler) attachImportedSkills(
	ctx context.Context, schema string, entityVersionID int,
	references []any, imported map[string]importedSkill,
) []string {
	messages := make([]string, 0)
	if len(references) == 0 {
		return messages
	}

	// THE PRECONDITION THIS COUNTER DEPENDS ON: `entityVersionID` names a version
	// this same request has just created, so it carries no attachment yet. Both
	// call sites hold it — the import attaches in phase 3 to a version phase 1
	// inserted, and the fork attaches to the version it inserted a few lines
	// above. A caller that attaches to an EXISTING version must read the current
	// count out of `entity_skill_mapping` first, or the cap below counts only
	// what this request added.
	//
	// The cap is measured over (entity_version_id, entity_type), which is the set
	// the attach route (internal/infra/db/repos/skills.go:239-241) and pylon's
	// `validate_agent_skill_limit` both measure. Every row this function writes
	// carries `entity_type = agent`, because the refusal below admits no other
	// value, so one counter measures that set exactly.
	attached := 0

	for _, raw := range references {
		reference, isMap := raw.(map[string]any)
		if !isMap {
			messages = append(messages, "unable to link a skill: the reference is not a JSON object")
			continue
		}
		importUUID, _ := reference["import_uuid"].(string)
		skill, found := imported[importUUID]
		if !found {
			// The case task 4 of the issue names. The reference points at a
			// skill the file did not carry, or at one whose own import failed,
			// so there is nothing in this project to attach.
			messages = append(messages, fmt.Sprintf(
				"unable to link skill %q: it is not among the imported skills", importUUID))
			continue
		}
		versionName, _ := reference["version_name"].(string)
		skillVersionID, resolved := skill.versionID(versionName)
		if !resolved {
			messages = append(messages, fmt.Sprintf(
				"unable to link skill %q: it carries no version to attach", importUUID))
			continue
		}
		// `agent` is the whole set. Pylon closes the enum to that one member and
		// types the request field as it, so any other value is a 400 there and on
		// the sibling write route here (internal/api/v2/skills/handler.go,
		// relationEntityType). Both READS filter on the literal — the chat read
		// (internal/db/queries/agent_chat.sql:132) and the attached-skill
		// registry (internal/api/v2/applications/handler.go) — so a row written
		// with any other value is a row nothing reads. The import used to write
		// whatever the file said, which made it the one path that could create
		// such a row, answer 201, and report it as attached.
		entityType, _ := reference["entity_type"].(string)
		if entityType == "" {
			entityType = v2skills.SkillEntityTypeAgent
		}
		if entityType != v2skills.SkillEntityTypeAgent {
			messages = append(messages, fmt.Sprintf(
				"unable to link skill %q: entity_type must be %q",
				importUUID, v2skills.SkillEntityTypeAgent))
			continue
		}
		// The cap the attach route enforces (v2skills.MaxSkillsPerEntityVersion,
		// pylon's MAX_SKILLS_PER_AGENT). The read side renders it as "n/5 skills
		// added", so an import that walked past it would make that counter say
		// 6/5 and the skill menu would stay disabled.
		if attached >= v2skills.MaxSkillsPerEntityVersion {
			messages = append(messages, fmt.Sprintf(
				"unable to link skill %q: a version carries at most %d skills",
				importUUID, v2skills.MaxSkillsPerEntityVersion))
			continue
		}
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
			VALUES ($1, $2, $3, $4)`, schema),
			entityVersionID, entityType, skill.id, skillVersionID); err != nil {
			slog.ErrorContext(ctx, "import: skill attachment insert failed",
				"schema", schema, "entity_version_id", entityVersionID,
				"skill_id", skill.id, "error", err)
			messages = append(messages, fmt.Sprintf(
				"unable to link skill %q: %s", importUUID, err.Error()))
			continue
		}
		attached++
	}
	return messages
}

// versionSkillReferences reads the `skills` array off one version entry.
//
// An ABSENT key leaves the version alone, which is the rule the `variables`
// branch of the import already follows and the rule that keeps every file
// written before this change importable.
func versionSkillReferences(version map[string]any) []any {
	references, _ := version["skills"].([]any)
	return references
}

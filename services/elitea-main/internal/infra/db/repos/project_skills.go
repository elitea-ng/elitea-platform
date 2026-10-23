package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/foldervisibility"
)

// ProjectSkillsRepo reads the project skills a chat turn picked by id, for
// the composer's `~` menu "Project skills" section.
//
// What it reads is the `base` version only — every skills read in Go joins
// skill_versions on name = 'base' — in the tenant schema of the conversation's
// project, so an id that belongs to another project is simply not found. A
// skill that folder access hides from the caller is excluded by the same
// predicate the skills routes use, so the caller sees exactly what the skills
// screen would have shown them.
type ProjectSkillsRepo struct {
	pool *pgxpool.Pool
}

func NewProjectSkillsRepo(pool *pgxpool.Pool) *ProjectSkillsRepo {
	return &ProjectSkillsRepo{pool: pool}
}

var _ agentexecutionapp.CurrentProjectSkillResolver = (*ProjectSkillsRepo)(nil)

// ResolveCurrentProjectSkills answers the skills in the order of skillIDs.
// Any id that is missing, hidden from the caller or without instructions is
// ErrProjectSkillUnknown: the same answer for all three, so the response does
// not tell a caller which ids exist. The caller is the user in ctx.
func (r *ProjectSkillsRepo) ResolveCurrentProjectSkills(
	ctx context.Context,
	projectID int64,
	skillIDs []int64,
) ([]agentexecutionapp.CurrentProjectSkill, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("project skills: no pool")
	}
	if projectID <= 0 || len(skillIDs) == 0 {
		return nil, agentexecutionapp.ErrProjectSkillUnknown
	}
	schema, err := tenantSchema(fmt.Sprintf("%d", projectID))
	if err != nil {
		return nil, err
	}
	access, err := foldervisibility.Resolve(ctx, r.pool, schema)
	if err != nil {
		return nil, fmt.Errorf("project skills: folder visibility: %w", err)
	}
	predicate := ""
	args := []any{skillIDs}
	if access.FolderRestrictions {
		predicate = " AND " + foldervisibility.ExclusionSQL(schema, "sk.id", "$2", "$3")
		args = append(args, []string{"skill"}, access.ActorID)
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT sk.id, sv.id, sk.name, COALESCE(sk.description, ''), COALESCE(sv.instructions, ''),
       COALESCE(sv.meta -> 'icon_meta', 'null'::jsonb)
FROM %[1]s.skills sk
JOIN %[1]s.skill_versions sv ON sv.skill_id = sk.id AND sv.name = 'base'
WHERE sk.id = ANY($1::int[])%[2]s`, schema, predicate), args...)
	if err != nil {
		return nil, fmt.Errorf("project skills: read: %w", err)
	}
	defer rows.Close()
	found := make(map[int64]agentexecutionapp.CurrentProjectSkill, len(skillIDs))
	for rows.Next() {
		var skill agentexecutionapp.CurrentProjectSkill
		var skillID, versionID int32
		var icon []byte
		if err := rows.Scan(&skillID, &versionID, &skill.Name, &skill.Description, &skill.Instructions, &icon); err != nil {
			return nil, fmt.Errorf("project skills: scan: %w", err)
		}
		skill.SkillID, skill.BaseVersionID = int64(skillID), int64(versionID)
		skill.IconMeta = json.RawMessage(icon)
		found[skill.SkillID] = skill
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project skills: read: %w", err)
	}
	result := make([]agentexecutionapp.CurrentProjectSkill, 0, len(skillIDs))
	for _, id := range skillIDs {
		skill, ok := found[id]
		if !ok || skill.Instructions == "" {
			return nil, agentexecutionapp.ErrProjectSkillUnknown
		}
		result = append(result, skill)
	}
	return result, nil
}

package repos

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
)

// skillTrustStates answers, in two queries for any number of skills, what the
// project trusts of each skill's current base text (ADR-0051 amendment 1, A1.6):
// the state a skill list row and a skill read carry, so no client asks once per
// row. A skill with no base version has no entry. Trust applies to the base
// text, the same text the trust routes name by revision.
func skillTrustStates(ctx context.Context, pool *pgxpool.Pool, schema, projectID string, ids []int64) (map[int64]skills.SkillTrust, error) {
	states := make(map[int64]skills.SkillTrust, len(ids))
	project, err := strconv.ParseInt(projectID, 10, 64)
	if len(ids) == 0 || err != nil || project <= 0 {
		return states, nil
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`
SELECT skill_id, COALESCE(meta ->> 'parent_project_id', ''), COALESCE(instructions, '')
FROM %s.skill_versions WHERE skill_id = ANY($1::bigint[]) AND name = 'base'`, schema), ids)
	if err != nil {
		return nil, fmt.Errorf("skill trust states: read lineage: %w", err)
	}
	defer rows.Close()
	text := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var parent, instructions string
		if err := rows.Scan(&id, &parent, &instructions); err != nil {
			return nil, fmt.Errorf("skill trust states: scan lineage: %w", err)
		}
		publisher, foreign := domain.Foreign(parent, project)
		state := skills.SkillTrust{Foreign: foreign, State: "not_required"}
		if foreign {
			state.State = "untrusted"
			if publisher > 0 {
				state.PublisherProjectID = &publisher
			}
			text[id] = instructions
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skill trust states: read lineage: %w", err)
	}
	if len(text) == 0 {
		return states, nil
	}
	foreignIDs := make([]int64, 0, len(text))
	for id := range text {
		foreignIDs = append(foreignIDs, id)
	}
	trusted, err := pool.Query(ctx, fmt.Sprintf(`
SELECT skill_id, revision FROM %s.skill_trust WHERE skill_id = ANY($1::bigint[]) AND revoked_at IS NULL`, schema), foreignIDs)
	if err != nil {
		return nil, fmt.Errorf("skill trust states: read trust: %w", err)
	}
	defer trusted.Close()
	active := map[int64][]string{}
	for trusted.Next() {
		var id int64
		var revision string
		if err := trusted.Scan(&id, &revision); err != nil {
			return nil, fmt.Errorf("skill trust states: scan trust: %w", err)
		}
		active[id] = append(active[id], revision)
	}
	if err := trusted.Err(); err != nil {
		return nil, fmt.Errorf("skill trust states: read trust: %w", err)
	}
	for id, instructions := range text {
		if domain.Trusted(instructions, active[id]) {
			state := states[id]
			state.State = "trusted"
			states[id] = state
		}
	}
	return states, nil
}

// withTrust sets Trust on each of items that has a state.
func withTrust(items []skills.Skill, states map[int64]skills.SkillTrust) {
	for i := range items {
		id, err := strconv.ParseInt(items[i].ID, 10, 64)
		if err != nil {
			continue
		}
		if state, ok := states[id]; ok {
			items[i].Trust = &state
		}
	}
}

package repos

import "fmt"

// createSkillSQL binds the owning project and authenticated author separately.
func createSkillSQL(schema string) string {
	return fmt.Sprintf(`
		INSERT INTO %s.skills (name, description, owner_id, author_id, uuid, meta)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), '{}')
		RETURNING id, name, COALESCE(description, ''), created_at`, schema)
}

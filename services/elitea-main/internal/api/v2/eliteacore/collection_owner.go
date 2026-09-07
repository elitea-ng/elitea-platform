package eliteacore

import "fmt"

// createCollectionInsertSQL builds the INSERT that CreateCollection runs.
//
// # Why owner_id and author_id cannot share a placeholder
//
// The statement was `VALUES ($1, $2, $3, $3, 'active', '{}')`, and $3 was the
// caller's user id. One placeholder therefore filled two columns that hold two
// different kinds of number (issue #533):
//
//   - `prompt_collections.owner_id` is the owning PROJECT. The legacy runtime
//     set both columns in one line —
//     `data["owner_id"], data["author_id"] = project_id, author_id`
//     (legacy/plugins/elitea_core/api/v2/collections.py:105) — the create
//     helper writes `"owner_id": project_id` (utils/collections.py:220), and
//     the read takes the project back out of the column:
//     `project_id = data['owner_id']` (utils/collections.py:421).
//   - `prompt_collections.author_id` is the USER. The listing resolves it with
//     `get_authors_data([i.author_id for i in collections])`
//     (api/v2/collections.py:54), which reads the user table.
//
// So every collection this service created claimed that a user was its owning
// project. The value satisfied the column type, and only a reader who joined
// the column could see it.
//
// The two placeholders are $3 for the project and $4 for the user. The values
// come from tenantOwnerID and from the principal, and their Go types
// (ownership.ProjectID and ownership.UserID) keep them apart at the call site.
//
// The table is not in this repository's migration corpus: no migration creates
// it, and the legacy runtime carries an admin task that drops it
// (methods/admin_tasks.py:2481-2566). migrations/tenant/0131 therefore probes
// for the table before it records the meanings on the two columns.
func createCollectionInsertSQL(schema string) string {
	return fmt.Sprintf(`
		INSERT INTO %s.prompt_collections (name, description, owner_id, author_id, status, meta)
		VALUES ($1, $2, $3, $4, 'active', '{}')
		RETURNING id`, schema)
}

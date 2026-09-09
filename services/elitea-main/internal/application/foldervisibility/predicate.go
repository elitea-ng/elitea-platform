package foldervisibility

import "fmt"

// ExclusionSQL accepts trusted SQL fragments. Bind kinds and actor values separately.
func ExclusionSQL(schema, entityIDSQL, kindsPlaceholder, actorPlaceholder string) string {
	return fmt.Sprintf(`NOT EXISTS (
 SELECT 1 FROM %[1]s.social_folder_items fi
 JOIN %[1]s.folder_access_overrides fo ON fo.folder_id=fi.folder_id
 WHERE fi.entity=ANY(%[3]s::text[]) AND fi.entity_id=%[2]s
 AND fo.user_id=%[4]s AND fo.access_level='no_access')`, schema, entityIDSQL, kindsPlaceholder, actorPlaceholder)
}

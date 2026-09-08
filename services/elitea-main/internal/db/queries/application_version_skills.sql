-- The tenant executor installs a transaction-local search_path. These queries
-- never accept a schema from the caller.

-- name: CopyApplicationVersionSkills :execrows
INSERT INTO entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
SELECT target.id, mapping.entity_type, mapping.skill_id, mapping.skill_version_id
FROM application_versions AS source
JOIN application_versions AS target ON target.application_id = source.application_id
JOIN entity_skill_mapping AS mapping ON mapping.entity_version_id = source.id
WHERE source.id = sqlc.arg(source_version_id)::integer
  AND target.id = sqlc.arg(target_version_id)::text::integer
  AND source.id <> target.id
ORDER BY mapping.id;

-- entity_skill_mapping is polymorphic and has no version FK. Remove only the
-- bindings of the version actually deleted, including on create compensation.
-- name: DeleteApplicationVersionWithSkills :one
WITH deleted AS (
    DELETE FROM application_versions
    WHERE application_id = sqlc.arg(application_id)::text::integer
      AND id = sqlc.arg(version_id)::text::integer
    RETURNING id
), deleted_skills AS (
    DELETE FROM entity_skill_mapping
    WHERE entity_version_id IN (SELECT id FROM deleted)
)
SELECT count(*)::bigint FROM deleted;

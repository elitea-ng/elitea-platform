// Package legacyshape holds the legacy-built database shape the upgrade
// tests start from. It imports nothing, so migrate's own tests can use it.
package legacyshape

// BuiltSQL turns a database carrying the legacy-era schema
// (internal/infra/db/migrations/001_initial.sql, which builds p_1) into the
// shape the legacy runtime left behind, with rows written before an upgrade.
// Apply it BEFORE migrate.Bootstrap and the numbered histories.
//
// The shape is the one measured on the real-model dump
// (product-real-models-main-d5e966074): centry.audit_events without the five
// token and cost columns, and a tenant schema whose elitea_tools has no
// owner_id, env_vars or uuid and which has no index_meta, social_likes,
// social_pins or social_feedbacks. Two audit rows and two toolkits (ids 1 and
// 2) are written in that shape.
const BuiltSQL = `ALTER TABLE centry.audit_events
    DROP COLUMN input_tokens,
    DROP COLUMN output_tokens,
    DROP COLUMN llm_cost,
    DROP COLUMN token_source,
    DROP COLUMN cost_source;
INSERT INTO centry.audit_events (timestamp, event_type, action, duration_ms, is_error, user_email, project_id)
VALUES ('2026-10-01 10:00:00+00', 'api', 'legacy row one', 1.5, false, 'one@example.com', 1),
       ('2026-10-01 10:00:01+00', 'api', 'legacy row two', 2.5, true, 'two@example.com', 1);

DROP TABLE p_1.index_meta;
DROP TABLE p_1.social_likes;
DROP TABLE p_1.social_pins;
DROP TABLE p_1.social_feedbacks;
ALTER TABLE p_1.elitea_tools
    DROP COLUMN owner_id,
    DROP COLUMN env_vars,
    DROP COLUMN uuid;
INSERT INTO p_1.elitea_tools (name, type, description, author_id, settings, meta)
VALUES ('legacy toolkit one', 'github', 'from the legacy runtime', 7, '{"a": 1}', '{}'),
       ('legacy toolkit two', 'jira', NULL, 8, '{}', '{"b": 2}');
`

-- 0286_marketplace_teams.sql — saved teams in the Marketplace (ADR-0051
-- amendment "Teams in the Marketplace", issue 3137).
--
-- A team listing is kind `team`. Its source is the saved team
-- (elitea_chat.agent_teams, shared 0233): source_project_id is the team's
-- project and source_entity_id the team id. It has no public catalogue twin, so
-- entity_id stays null. Its listing versions snapshot the team's definition:
-- name, description, floor policy, lead position and the members as (member
-- listing id, member listing version id, position). Its requirements are the
-- union of the members' requirements. listing_versions.version_id of a team
-- version is a per-listing sequence number (1, 2, ...), not a catalogue id:
-- a team has no catalogue row.
--
-- A team hire records one install of entity_kind `team` whose entity_id is the
-- agent_teams id it created, besides the installs of the member agents.
--
-- Shared 0250 is released and immutable, so the two kind checks are replaced
-- here. Widening only: every existing row still passes. No permission: a team
-- is published with the agent publish permission and hired with the agent
-- hire permission and the team create permission. No BEGIN/COMMIT: the
-- ledgered runner wraps the file.

ALTER TABLE elitea_marketplace.listings
    DROP CONSTRAINT IF EXISTS listings_kind_check;
ALTER TABLE elitea_marketplace.listings
    ADD CONSTRAINT listings_kind_check CHECK (kind IN ('agent', 'skill', 'team'));

ALTER TABLE elitea_marketplace.installs
    DROP CONSTRAINT IF EXISTS installs_entity_kind_check;
ALTER TABLE elitea_marketplace.installs
    ADD CONSTRAINT installs_entity_kind_check CHECK (entity_kind IN ('agent', 'skill', 'team'));

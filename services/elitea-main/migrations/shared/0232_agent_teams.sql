-- 0232_agent_teams.sql — saved teams (rosters) of agents (ADR-0050
-- decision 8, issue 1620).
--
-- A TEAM is a project's saved roster: the agents of a conversation, their
-- floor policy and the lead, kept so a person can start a new conversation
-- with the same agents. A team adds no capability. It names agents; it grants
-- nothing. Starting a conversation from a team adds its agents through the
-- ordinary participant path (the same composition and project-access checks
-- an "add participant" runs), so a person can start from a team only what
-- they could have assembled by hand, and each agent keeps its own grants.
--
-- elitea_chat.agent_teams
--   One team of one project. `name` is unique per project, case-insensitively.
--   floor_policy      addressed | lead, the conversation floor of
--                     internal/domain/turntaking (meta.floor.policy).
--   lead_position     the member that leads (its agent_team_members
--                     position), set exactly when floor_policy is lead. A
--                     conversation started from the team gets
--                     meta.floor.lead_participant_id of that agent's
--                     participant.
--   created_by        the user who saved it. Not a foreign key: users are
--                     central (auth_core__user) and outlive no team.
--
-- elitea_chat.agent_team_members
--   The roster, in order. One row per agent: application_id and the project
--   that owns it (agent_project_id, the participant's entity_meta.project_id;
--   the team's own project or the public catalogue project), and the version
--   the conversation runs (version_id, the participant's
--   entity_settings.version_id). `name` is the agent's name when the team was
--   saved, shown while the agent cannot be read. At most 16 members
--   (turntaking.MaxAddressed).
--   No foreign key to the agent: an agent lives in a tenant schema, and a
--   deleted or unpublished agent makes the start refuse with
--   team_member_unavailable rather than silently drop it.
--
-- WHY SHARED, NOT TENANT. The permission strings must be seeded here anyway
-- (a new central permission needs a NEW shared migration: 0060 returns early
-- on an existing database), and one central table keyed by project keeps the
-- whole feature in one file, as elitea_chat.shared_chat_links (0103) does.
-- project_id is a column, never a schema name, so no caller input picks a
-- schema, and deleting the project deletes its teams (ON DELETE CASCADE).
--
-- THE PERMISSIONS
--   models.teams.list     list and read teams; with the chat permissions,
--                         start a conversation from one
--                         (default mode: admin, editor, viewer)
--   models.teams.create   save a team, from scratch or from a conversation
--   models.teams.update   edit a team
--   models.teams.delete   delete a team
--                         (the three: default mode admin, editor)
-- A project whose matrix was saved gets the strings on its roles of the same
-- names, as 0229 does: the strings never existed, so no saved matrix
-- withheld them on purpose. internal/infra/db/migrations/001_initial.sql
-- seeds the same grants on a fresh database.
--
-- Additive only. Idempotent. Every grant tolerates a database without the
-- auth_core tables. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_chat;

CREATE TABLE IF NOT EXISTS elitea_chat.agent_teams (
    id                  bigserial   PRIMARY KEY,
    project_id          integer     NOT NULL REFERENCES centry.project (id) ON DELETE CASCADE,
    name                text        NOT NULL
                        CHECK (btrim(name) <> '' AND char_length(name) <= 128),
    description         text        NOT NULL DEFAULT ''
                        CHECK (char_length(description) <= 2000),
    floor_policy        text        NOT NULL DEFAULT 'addressed'
                        CHECK (floor_policy IN ('addressed', 'lead')),
    lead_position       integer     CHECK (lead_position IS NULL OR lead_position BETWEEN 1 AND 16),
    created_by          bigint      NOT NULL CHECK (created_by > 0),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_teams_lead_matches_policy
        CHECK ((floor_policy = 'lead') = (lead_position IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS agent_teams_project_name_key
    ON elitea_chat.agent_teams (project_id, lower(name));

CREATE TABLE IF NOT EXISTS elitea_chat.agent_team_members (
    team_id          bigint  NOT NULL REFERENCES elitea_chat.agent_teams (id) ON DELETE CASCADE,
    position         integer NOT NULL CHECK (position BETWEEN 1 AND 16),
    application_id   bigint  NOT NULL CHECK (application_id > 0),
    agent_project_id bigint  NOT NULL CHECK (agent_project_id > 0),
    version_id       bigint  NOT NULL CHECK (version_id > 0),
    name             text    NOT NULL DEFAULT '' CHECK (char_length(name) <= 256),
    PRIMARY KEY (team_id, position),
    UNIQUE (team_id, agent_project_id, application_id)
);

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0232: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- The mode spelled as a literal, one per INSERT: the router's grant
    -- gate (router_permission_grant_gate_test.go) reads the grants of this
    -- directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'models.teams.list'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
    ON CONFLICT (role_id, permission) DO NOTHING;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        ('models.teams.create'),
        ('models.teams.update'),
        ('models.teams.delete')
    ) AS grant_row(permission)
    WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
    ON CONFLICT (role_id, permission) DO NOTHING;

    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, grant_row.permission
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    CROSS JOIN (VALUES
        ('models.teams.list', 'admin'),
        ('models.teams.list', 'editor'),
        ('models.teams.list', 'viewer'),
        ('models.teams.create', 'admin'),
        ('models.teams.create', 'editor'),
        ('models.teams.update', 'admin'),
        ('models.teams.update', 'editor'),
        ('models.teams.delete', 'admin'),
        ('models.teams.delete', 'editor')
    ) AS grant_row(permission, role_name)
    WHERE project_role.name = grant_row.role_name
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;

-- SQLC compiler projection for the current EliteA tenant application-version
-- table.
--
-- This file is NOT a runtime migration. The table already exists in each
-- p_<project_id> schema and is owned by the current tenant-schema lifecycle.
-- Keep this projection aligned with the deployed PostgreSQL shape.

CREATE TABLE applications (
    id serial PRIMARY KEY,
    name varchar(128) NOT NULL,
    description varchar(2304),
    owner_id integer NOT NULL,
    updated_at timestamp NOT NULL DEFAULT now()
);

CREATE TABLE application_versions (
    id serial PRIMARY KEY,
    application_id integer NOT NULL,
    name varchar(128) NOT NULL,
    status varchar NOT NULL,
    author_id integer NOT NULL,
    uuid uuid NOT NULL UNIQUE,
    created_at timestamp NOT NULL DEFAULT now(),
    shared_owner_id integer,
    shared_id integer,
    llm_settings json NOT NULL,
    instructions varchar,
    conversation_starters json NOT NULL,
    welcome_message varchar NOT NULL,
    agent_type varchar NOT NULL,
    meta jsonb NOT NULL,
    pipeline_settings jsonb NOT NULL,
    UNIQUE (application_id, name),
    UNIQUE (shared_owner_id, shared_id)
);

-- The version's authored variables. This is the AUTHORITATIVE store for them:
-- pylon has no other (`ApplicationVersion.variables` is a relationship to
-- `ApplicationVariable`), the version-detail GET, `GetVersionExpanded` and the
-- export all read it, and `application_version_details_json` in
-- internal/db/queries/agent_chat.sql reads it to serve a worker. `meta ->
-- 'variables'` is only a mirror; see that projection's comment for the rule.
CREATE TABLE application_variables (
    id serial PRIMARY KEY,
    application_version_id integer NOT NULL REFERENCES application_versions(id) ON DELETE CASCADE,
    name varchar NOT NULL,
    value varchar,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    UNIQUE (application_version_id, name)
);

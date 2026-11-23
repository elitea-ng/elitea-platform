-- SQLC compiler input for the current-baseline public.auth_core__* schema captured in
-- testdata/postgres/legacy-centry-catalog.json on 2026-07-19.
--
-- This file is NOT a runtime migration and MUST NOT be executed by service
-- startup. elitea-migrate remains the only target-schema migration owner. This
-- projection lets sqlc type-check queries against the existing populated
-- legacy auth schema while adoption and upgrade migrations are built.

CREATE TABLE public.auth_core__user (
    id serial PRIMARY KEY,
    email text,
    name text,
    last_login timestamp without time zone,
    suspended boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX ix_auth_core__user_email
    ON public.auth_core__user (email);

CREATE TABLE public.auth_core__user_provider (
    user_id integer NOT NULL
        REFERENCES public.auth_core__user (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    provider_ref text NOT NULL,
    PRIMARY KEY (user_id, provider_ref),
    UNIQUE (provider_ref)
);

CREATE TABLE public.auth_core__group (
    id serial PRIMARY KEY,
    parent_id integer
        REFERENCES public.auth_core__group (id)
        ON UPDATE CASCADE ON DELETE SET NULL,
    name text
);

CREATE TABLE public.auth_core__group_provider (
    group_id integer NOT NULL
        REFERENCES public.auth_core__group (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    provider_ref text NOT NULL,
    PRIMARY KEY (group_id, provider_ref),
    UNIQUE (provider_ref)
);

CREATE TABLE public.auth_core__user_group (
    user_id integer NOT NULL
        REFERENCES public.auth_core__user (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    group_id integer NOT NULL
        REFERENCES public.auth_core__group (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    PRIMARY KEY (user_id, group_id)
);

CREATE TABLE public.auth_core__scope (
    id serial PRIMARY KEY,
    parent_id integer
        REFERENCES public.auth_core__scope (id)
        ON UPDATE CASCADE ON DELETE SET NULL,
    name text
);

CREATE TABLE public.auth_core__user_permission (
    user_id integer NOT NULL
        REFERENCES public.auth_core__user (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    scope_id integer NOT NULL
        REFERENCES public.auth_core__scope (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL,
    PRIMARY KEY (user_id, scope_id, permission)
);

CREATE TABLE public.auth_core__group_permission (
    group_id integer NOT NULL
        REFERENCES public.auth_core__group (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    scope_id integer NOT NULL
        REFERENCES public.auth_core__scope (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL,
    PRIMARY KEY (group_id, scope_id, permission)
);

CREATE TABLE public.auth_core__token (
    id serial PRIMARY KEY,
    uuid varchar(36),
    expires timestamp without time zone,
    user_id integer
        REFERENCES public.auth_core__user (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    name text,
    -- shared 0265 (ADR-0059 R1.1): administration access of the credential.
    admin_access text NOT NULL DEFAULT 'none'
        CONSTRAINT auth_core__token_admin_access_check
        CHECK (admin_access IN ('none', 'scoped', 'inherit')),
    admin_scopes text[] NOT NULL DEFAULT '{}',
    CONSTRAINT auth_core__token_admin_scopes_check
        CHECK (admin_access = 'scoped' OR cardinality(admin_scopes) = 0)
);

CREATE UNIQUE INDEX ix_auth_core__token_uuid
    ON public.auth_core__token (uuid);

CREATE TABLE public.auth_core__role (
    id serial PRIMARY KEY,
    name varchar(64) NOT NULL,
    mode varchar(64) NOT NULL,
    UNIQUE (name, mode)
);

CREATE TABLE public.auth_core__role_permission (
    id serial PRIMARY KEY,
    role_id integer NOT NULL
        REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
    permission varchar(64),
    UNIQUE (role_id, permission)
);

CREATE TABLE public.auth_core__user_role (
    id serial PRIMARY KEY,
    user_id integer NOT NULL
        REFERENCES public.auth_core__user (id) ON DELETE CASCADE,
    role_id integer NOT NULL
        REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
    UNIQUE (user_id, role_id)
);

CREATE TABLE public.auth_core__project_role (
    id serial PRIMARY KEY,
    project_id integer NOT NULL,
    name text NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE public.auth_core__project_role_permission (
    id serial PRIMARY KEY,
    project_id integer NOT NULL,
    role_id integer
        REFERENCES public.auth_core__project_role (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL,
    UNIQUE (project_id, role_id, permission)
);

CREATE TABLE public.auth_core__project_user_role (
    id serial PRIMARY KEY,
    project_id integer NOT NULL,
    user_id integer
        REFERENCES public.auth_core__user (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    role_id integer
        REFERENCES public.auth_core__project_role (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    UNIQUE (project_id, user_id, role_id)
);

-- The history every central role write records (internal/adminaccess/
-- rolegrants, issue 2499). migrations/shared/0260_admin_role_grants.sql is the
-- target-schema authority and adds the append-only trigger; this is the same
-- table, so every fixture built on this projection can run the role writes.
CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.admin_role_grants (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       integer     NOT NULL,
    role_name     text        NOT NULL,
    role_mode     text        NOT NULL,
    action        text        NOT NULL,
    actor_user_id integer,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    reason        text,
    source        text        NOT NULL,
    source_ref    text,
    CONSTRAINT admin_role_grants_action_known
        CHECK (action IN ('granted', 'revoked')),
    CONSTRAINT admin_role_grants_source_known
        CHECK (source IN ('console', 'api', 'scim', 'migration', 'bootstrap', 'operator', 'review')),
    CONSTRAINT admin_role_grants_role_present
        CHECK (length(btrim(role_name)) > 0 AND length(btrim(role_mode)) > 0),
    CONSTRAINT admin_role_grants_actor_matches_source
        CHECK (
            (source IN ('console', 'api') AND actor_user_id IS NOT NULL)
            OR (source IN ('bootstrap', 'migration') AND actor_user_id IS NULL)
            OR source IN ('scim', 'operator', 'review')
        )
);

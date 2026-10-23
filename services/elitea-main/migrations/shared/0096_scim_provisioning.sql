-- 0096_scim_provisioning.sql — the side table SCIM 2.0 user provisioning needs.
--
-- WHAT SCIM IS FOR HERE. An identity provider (Okta, Entra ID, OneLogin) that
-- federates logins can also PUSH the directory: create an account when a person
-- joins, update it when their name or address changes, and deactivate it when
-- they leave. Without it, an account is created on first login and never
-- removed — a person who leaves the company keeps a working account until
-- somebody remembers to suspend it by hand.
--
-- Nothing in this platform did SCIM before this migration, and neither did
-- the legacy runtime: `grep -ril scim legacy/plugins/` finds nothing. This is
-- not a port.
--
-- WHY A SIDE TABLE AND NOT COLUMNS ON `auth_core__user`. That table is owned by
-- the legacy runtime schema (internal/infra/db/migrations/001_initial.sql seeds
-- it, and a legacy-managed deployment creates it). Claiming a legacy-owned
-- table in a shared migration has broken this repository's seeds three times
-- with a 42P07, and the rule that came out of it is: read and UPDATE those
-- rows, never change their shape. So the SCIM-specific facts live here, keyed
-- by the user id.
--
-- The person's identity itself stays where it already is:
--
--   * `auth_core__user.email`     is the SCIM `userName` and primary email.
--   * `auth_core__user.name`      is the SCIM `displayName`.
--   * `auth_core__user.suspended` is the inverse of the SCIM `active` flag.
--
-- Only `externalId` and the resource timestamps have nowhere to go, and they are
-- what this table holds.
--
-- EXTERNAL_ID IS THE IDENTITY PROVIDER'S OWN KEY, and it is UNIQUE. SCIM clients
-- use it to find the resource they created when they have lost the local id, and
-- two accounts claiming one external id would make that lookup ambiguous — the
-- client would update whichever the database returned first.
--
-- WHY SHARED, NOT TENANT. A directory is platform-wide: the identity provider
-- pushes people, not project members, and the accounts it creates exist before
-- any project selects them.
--
-- NO FOREIGN KEY, and this is the same rule 0094 and 0095 state. It is not a
-- preference: the shared corpus MUST be applicable on its own. `migrate.
-- ApplyShared` runs this set against a database that may hold nothing else —
-- every `dbtest` template and several integration harnesses do exactly that —
-- and `auth_core__user` is created by a DIFFERENT corpus
-- (internal/infra/db/migrations/001_initial.sql) or by the legacy runtime's own
-- migrations. A `REFERENCES auth_core__user(id)` here made 0096 fail with a
-- 42P01 on every such database, which took six unrelated artifact suites down
-- with it.
--
-- WHAT THE FOREIGN KEY WOULD HAVE BOUGHT was `ON DELETE CASCADE`. Without it, an
-- operator who hard-deletes an account from the admin Users page leaves a row
-- here whose `user_id` names nothing. Such a row is INERT for every read: each
-- one joins from `auth_core__user` outwards, so a row with no account is never
-- returned. The one thing it could still do is hold an `external_id` the
-- identity provider later re-uses, so `upsertSCIMFacts` clears a stale claim
-- whose account is gone before it inserts. See internal/scimdirectory.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.scim_users (
    -- user_id is the primary key: one SCIM record per account, and the account
    -- is the thing that already exists. It is NOT a foreign key; see the header
    -- for why, and for what replaces the cascade.
    user_id     integer     PRIMARY KEY,
    -- external_id is the identity provider's own identifier for the person. It
    -- is optional: a client MAY omit it, and RFC 7643 says a service provider
    -- must not require one.
    external_id text        NOT NULL DEFAULT '',
    -- created_at and updated_at are the SCIM `meta.created` and
    -- `meta.lastModified`. auth_core__user carries neither, and a resource with
    -- no timestamps cannot answer the conditional requests a client makes.
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- One account per external id. An empty external_id is NOT constrained, because
-- an omitted identifier is legitimate and every such row would otherwise
-- collide with every other.
CREATE UNIQUE INDEX IF NOT EXISTS scim_users_external_id_uniq
    ON elitea_auth.scim_users (external_id)
    WHERE external_id <> '';

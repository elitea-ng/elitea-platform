-- 0251_marketplace_upgrades.sql — upgrades of installed agents and upstream
-- updates of forks (ADR-0051 decision 4, issue 1630, phase M4).
--
-- installs.upgrade_policy is the project's choice for one installed agent:
-- `pinned` (nothing changes until a person acts, the default), `notify` (a
-- card announces new versions) or `auto` (low tier only, and only when the new
-- version adds no tool, capability or requirement and does not raise the tier;
-- internal/marketplace/upgrade.go holds the rule). A fork ignores the column:
-- a fork is never updated without a person.
--
-- change_requests.marketplace_payload carries what an approved upgrade or
-- upstream merge applies and the request cannot hold in its other columns:
-- for an upgrade the bindings the person chose for the target version, for an
-- upstream merge the draft version the merge wrote and the resolution of each
-- field.
--
-- Additive only. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_marketplace.installs
    ADD COLUMN IF NOT EXISTS upgrade_policy text NOT NULL DEFAULT 'pinned'
        CHECK (upgrade_policy IN ('pinned', 'notify', 'auto'));

ALTER TABLE elitea_lifecycle.change_requests
    ADD COLUMN IF NOT EXISTS marketplace_payload jsonb NOT NULL DEFAULT '{}'::jsonb;

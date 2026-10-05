-- 0143_native_client_min_version.sql — a registered native client's own
-- minimum version (ADR-0025 WP4).
--
-- The deployment-wide minimum lives in the `native_client_policy` section of
-- centry.platform_config (no DDL: that table is generic key/value). This column
-- is the per-client one the discovery document publishes under
-- `min_client_version.<client_id>`: an iOS and an Android build of the same app
-- ship on different schedules, so one number cannot serve both. It can only
-- RAISE the deployment-wide minimum — the effective minimum is the higher of
-- the two — so a client row cannot exempt an app from a floor the policy sets.
--
-- '' means no per-client minimum. The application validates the grammar
-- (MAJOR.MINOR.PATCH[-pre][+build], internal/clientversion); the CHECK only
-- bounds the length, the same bound the X-Client-Version header gets.
--
-- No permission: the column is written through the native_clients admin
-- surface, gated on `configuration.native_clients` (shared 0141), which also
-- guards the `native_client_policy` section (coordinator decision 6).
--
-- 0142 is left unused on purpose: it was reserved for a separate device
-- registry migration that WP3 folded into 0141.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.

ALTER TABLE elitea_auth.native_clients
    ADD COLUMN IF NOT EXISTS min_client_version text NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'native_clients_min_client_version_length'
          AND conrelid = 'elitea_auth.native_clients'::regclass
    ) THEN
        ALTER TABLE elitea_auth.native_clients
            ADD CONSTRAINT native_clients_min_client_version_length
            CHECK (length(min_client_version) <= 64);
    END IF;
END
$$;

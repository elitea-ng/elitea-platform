-- 0179_workspace_handoff_forge_credential.sql — the forge credential a
-- thread's owner chose for the cloud executor (ADR-0034 decision 10,
-- amendment 2026-10-26).
--
-- A cloud turn pushes the thread's branch and opens a pull request (GitHub)
-- or merge request (GitLab) through a configuration of the project (or one
-- of the owner's private configurations). The owner picks it per thread,
-- from the web, the mobile app or the desktop.
--
--   forge_credential  NULL, or a configuration REFERENCE: its type and the
--                     reference a toolkit setting holds: {"type": "github"
--                     | "gitlab", "elitea_title": text, "private": bool}.
--                     Never a secret: Main expands it, as the owner, only
--                     when a running cloud turn of the thread asks over the
--                     claim-bound runtime plane.
--
-- A new send keeps the choice; the erasure clears it with the cloud copy.
-- No data: every existing row is NULL, which is what it was.
ALTER TABLE elitea_runtime.workspace_handoffs
    ADD COLUMN forge_credential jsonb NULL
        CONSTRAINT workspace_handoffs_forge_credential_shape CHECK (
            -- COALESCE: a missing key makes a test NULL, which a CHECK lets pass.
            forge_credential IS NULL OR COALESCE(
                jsonb_typeof(forge_credential) = 'object'
                AND forge_credential ->> 'type' IN ('github', 'gitlab')
                AND jsonb_typeof(forge_credential -> 'elitea_title') = 'string'
                AND jsonb_typeof(forge_credential -> 'private') = 'boolean'
                AND length(forge_credential ->> 'elitea_title') BETWEEN 1 AND 256
                -- exactly the three keys: nothing else (a token, say) fits
                AND forge_credential - 'type' - 'elitea_title' - 'private' = '{}'::jsonb,
                FALSE
            )
        );

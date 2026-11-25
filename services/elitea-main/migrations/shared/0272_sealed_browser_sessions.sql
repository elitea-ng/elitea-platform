-- 0272_sealed_browser_sessions.sql — the `sealed` session kind (ADR-0059
-- decision 5, "Path 2: a sealed credential when single sign-on is down", and
-- decision 7, the credentials table; story R2.7a, issue 2521).
--
-- A sealed sign-in (R2.7b) writes a row of elitea_auth.browser_sessions with
-- the provider `sealed`: it carries the elevation only, never talks to an
-- identity provider, is read from a cookie of its own (`__Host-elitea_sealed`)
-- on the administration surface only, and its absolute expiry is the
-- elevation's. The provider CHECK of shared 0117 lists the three planes that
-- existed then; this widens it by the fourth value. 0117 is applied and
-- immutable, so the constraint is replaced here, under the same name.
--
-- A widening: every row 0117's CHECK admitted is admitted by this one, and no
-- row is written. Re-applying drops and re-adds the same definition.
--
-- The guard keeps the file harmless on a database where 0117 has not run.

DO $$
BEGIN
    IF to_regclass('elitea_auth.browser_sessions') IS NULL THEN
        RAISE NOTICE '0272: elitea_auth.browser_sessions absent, no provider CHECK to widen';
        RETURN;
    END IF;
    ALTER TABLE elitea_auth.browser_sessions
        DROP CONSTRAINT IF EXISTS browser_sessions_provider_known;
    ALTER TABLE elitea_auth.browser_sessions
        ADD CONSTRAINT browser_sessions_provider_known
        CHECK (provider IN ('oidc', 'saml', 'form', 'sealed'));
END;
$$;

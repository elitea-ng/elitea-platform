-- 0263_recent_authentication_saml_and_form.sql — recent authentication for
-- SAML and Form sessions (ADR-0059 decision 5, path 1 step 4; story R1.10b,
-- issue 2594).
--
-- 0262 gave every server-side browser session the time of its person's last
-- authentication. For OIDC that time is the provider's `auth_time`; for SAML
-- the plane wrote the SIGN-IN time, which a silent single sign-on produces
-- without the person proving anything. ForceAuthn is what makes a SAML sign-in
-- a fresh authentication, and the Form plane had no time at all. Two columns:
--
--   elitea_auth.browser_sessions.authentication_proven
--       TRUE when the row's authenticated_at is PROOF of an authentication:
--       an OIDC id_token's auth_time, or a SAML assertion's AuthnInstant
--       answering a request that carried ForceAuthn="true". The
--       recent-authentication check (apimw.RecentAuthentication) admits only
--       such rows. NOT NULL DEFAULT false, and NO BACKFILL: an existing row
--       cannot say which kind of time 0262 gave it (a SAML row holds its
--       sign-in time), and a replica of the previous release, which does not
--       name the column, keeps writing rows during a rolling upgrade. Both
--       read as unproven, which only ever asks for one more sign-in.
--
--   elitea_auth.form_sessions.authenticated_at
--       when the person behind a Form session last entered their password: at
--       sign-in, and again at each re-entry (`/auth/form/reauthenticate`). It
--       is a column, not a member of the JSON record, because the previous
--       release decodes the record with unknown members refused and DELETES a
--       record it cannot decode. NULL is unknown (every session that exists
--       today, and a row a previous-release replica writes) and never counts
--       as a recent authentication.
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

ALTER TABLE elitea_auth.browser_sessions
    ADD COLUMN IF NOT EXISTS authentication_proven boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN elitea_auth.browser_sessions.authentication_proven IS
    'TRUE when authenticated_at is proof of an authentication (OIDC auth_time, SAML AuthnInstant under ForceAuthn). Only such rows satisfy a recent-authentication check (ADR-0059 R1.10b).';

ALTER TABLE elitea_auth.form_sessions
    ADD COLUMN IF NOT EXISTS authenticated_at timestamptz;

COMMENT ON COLUMN elitea_auth.form_sessions.authenticated_at IS
    'When the person behind this Form session last entered their password (sign-in or re-entry, ADR-0059 R1.10b). NULL is unknown and never counts as a recent authentication.';

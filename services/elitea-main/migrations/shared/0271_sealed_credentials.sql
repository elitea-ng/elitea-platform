-- 0271_sealed_credentials.sql — the sealed break-glass credentials (ADR-0059
-- decision 5, "Path 2: a sealed credential when single sign-on is down";
-- story R2.6a, issue 2520).
--
-- WHAT A SEALED CREDENTIAL IS. The governed way in when the identity provider
-- is down. An operator with access to the workload runs
-- `elitea-main break-glass issue`; the command prints `bg_<selector>.<verifier>`
-- ONCE and stores only what is below. The sign-in page (R2.7b) finds the row
-- by its selector, compares one Argon2id digest, consumes the row and starts an
-- elevation at the row's level.
--
-- ONE ROW PER CREDENTIAL:
--
--   id               uuid; what the admin.elevation events credential_issued,
--                    credential_used and credential_revoked name
--                    (credential_id) and what `break-glass revoke` takes
--   selector         random, not secret, unique: it finds the row. An unknown
--                    selector costs the page no hash
--   verifier_digest  the Argon2id digest of the verifier, in the PHC string
--                    form `$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>`, so the
--                    parameters travel with the digest. THE VERIFIER ITSELF
--                    IS NEVER STORED, in this table or anywhere else
--   label            what the custodians call it ("envelope A, safe 2");
--                    display text, never a secret
--   level            admin | super_admin: the level of the elevation the
--                    credential starts (the operator is the approver on this
--                    path)
--   user_id          the person the credential is bound to (auth_core__user.id;
--                    no foreign key, as for the elevation ledger: the shared
--                    corpus also runs where auth_core does not exist)
--   split            the verifier was printed as two halves for two
--                    custodians, so the page asks for both
--   issued_at        timestamptz
--   expires_at       at most 365 days after issued_at
--   used_at          set once, by the one-statement consume of R2.7b
--   revoked_at       set once, by `break-glass revoke` (and by R2.11 when the
--                    person is suspended, deleted or deactivated)
--
-- ROWS ARE NEVER DELETED, BY TRIGGER. The posture list and the review read the
-- history. DELETE and TRUNCATE are refused, everything but used_at and
-- revoked_at is frozen at INSERT, and each of those two is written once
-- (NULL -> a time).
--
-- The chart never creates a credential and never stores one in a Secret.

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.sealed_credentials (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    selector        text        NOT NULL,
    verifier_digest text        NOT NULL,
    label           text        NOT NULL DEFAULT '',
    level           text        NOT NULL,
    user_id         bigint      NOT NULL,
    split           boolean     NOT NULL DEFAULT false,
    issued_at       timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz,
    revoked_at      timestamptz,
    CONSTRAINT sealed_credentials_selector_key UNIQUE (selector),
    CONSTRAINT sealed_credentials_selector_shape
        CHECK (selector ~ '^[0-9a-f]{24}$'),
    CONSTRAINT sealed_credentials_digest_shape
        CHECK (verifier_digest ~ '^\$argon2id\$v=19\$m=[0-9]+,t=[0-9]+,p=[0-9]+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$'),
    CONSTRAINT sealed_credentials_label_bounded
        CHECK (length(label) <= 200),
    CONSTRAINT sealed_credentials_level_known
        CHECK (level IN ('admin', 'super_admin')),
    CONSTRAINT sealed_credentials_user_present
        CHECK (user_id > 0),
    -- The lifetime ceiling in code (ADR-0059 decision 5): 365 days.
    CONSTRAINT sealed_credentials_lifetime_bounded
        CHECK (expires_at > issued_at AND expires_at <= issued_at + interval '365 days'),
    CONSTRAINT sealed_credentials_used_after_issue
        CHECK (used_at IS NULL OR used_at >= issued_at),
    CONSTRAINT sealed_credentials_revoked_after_issue
        CHECK (revoked_at IS NULL OR revoked_at >= issued_at)
);

CREATE INDEX IF NOT EXISTS sealed_credentials_user_idx
    ON elitea_auth.sealed_credentials (user_id);

CREATE INDEX IF NOT EXISTS sealed_credentials_open_expiry_idx
    ON elitea_auth.sealed_credentials (expires_at)
    WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE OR REPLACE FUNCTION elitea_auth.sealed_credentials_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_row jsonb;
    new_row jsonb;
    frozen  text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- A credential is issued unused and unrevoked.
        IF NEW.used_at IS NOT NULL OR NEW.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.sealed_credentials: a credential cannot be inserted used or revoked'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'elitea_auth.sealed_credentials keeps its history: % refused', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    old_row := to_jsonb(OLD);
    new_row := to_jsonb(NEW);
    SELECT string_agg(key, ', ' ORDER BY key) INTO frozen
    FROM jsonb_each(old_row - 'used_at'::text - 'revoked_at'::text) AS o(key, value)
    WHERE new_row -> key IS DISTINCT FROM value;
    IF frozen IS NOT NULL THEN
        RAISE EXCEPTION 'elitea_auth.sealed_credentials keeps its history: an UPDATE of % refused', frozen
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at THEN
        RAISE EXCEPTION 'elitea_auth.sealed_credentials: the use is recorded once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION 'elitea_auth.sealed_credentials: the revocation is recorded once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS sealed_credentials_append_only ON elitea_auth.sealed_credentials;
CREATE TRIGGER sealed_credentials_append_only
    BEFORE INSERT OR UPDATE OR DELETE ON elitea_auth.sealed_credentials
    FOR EACH ROW EXECUTE FUNCTION elitea_auth.sealed_credentials_append_only();

DROP TRIGGER IF EXISTS sealed_credentials_no_truncate ON elitea_auth.sealed_credentials;
CREATE TRIGGER sealed_credentials_no_truncate
    BEFORE TRUNCATE ON elitea_auth.sealed_credentials
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_auth.sealed_credentials_append_only();

COMMENT ON TABLE elitea_auth.sealed_credentials IS
    'Sealed break-glass credentials (ADR-0059 R2.6a): selector and Argon2id digest of the verifier only; never deleted.';
COMMENT ON COLUMN elitea_auth.sealed_credentials.verifier_digest IS
    'Argon2id digest of the verifier in PHC form; the verifier itself is never stored.';

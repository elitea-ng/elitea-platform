-- 0242_token_name_limit.sql — the 128-character name limit of issue 1968 for
-- personal access tokens.
--
-- POST /token refuses a name over 128 characters (it was 768 bytes). A token
-- already stored with a longer name is truncated to its first 128 characters,
-- the owner's decision for existing data; the name is a label, not a key.
--
-- No CHECK constraint: auth_core__token is also written by the callback and
-- provider token paths, whose names are the platform's own (0159), and the
-- column carries no uniqueness a cut could break. The handler is the gate.
--
-- Guarded: the table belongs to the identity baseline and is absent in some
-- integration fixtures. Idempotent.
DO $$
BEGIN
    IF to_regclass('public.auth_core__token') IS NULL THEN
        RETURN;
    END IF;
    UPDATE public.auth_core__token
       SET name = left(name, 128)
     WHERE char_length(name) > 128;
END
$$;

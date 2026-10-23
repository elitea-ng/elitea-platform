-- Shared conversation links: the store behind "share a conversation by link".
--
-- WHY THIS IS A *SHARED* MIGRATION AND NOT A TENANT ONE
--
-- Every other chat object in this repository is tenant-scoped: chat_conversations,
-- chat_message_group and friends live in `p_<project_id>` (migrations/tenant/0123,
-- 0126, 0127). A share link is about a conversation, so tenant looks like the
-- obvious home. It is the wrong one, and the reason is the LOOKUP KEY.
--
-- The unauthenticated view endpoint is handed a token and NOTHING else — no
-- project, no session, no membership. Resolving a token against a tenant table
-- would mean either enumerating every `p_%` schema in the database on each
-- anonymous request (an unauthenticated caller could then drive a
-- schema-count-proportional scan by guessing tokens: a trivially available
-- amplification), or smuggling the project id into the token so the server can
-- pick the schema from caller-supplied input. The second is worse than the first:
-- it makes the token structured and therefore partially guessable, and it hands
-- an anonymous caller a way to steer which schema is queried.
--
-- One central table keyed on the token settles both. The project id is a COLUMN,
-- read out of the row the token resolved to, so the tenant schema the reader
-- then queries is server-derived and never caller-derived.
--
-- The schema is a NEW one this repository owns end to end (`elitea_chat`), in
-- the same style as elitea_auth (0095), elitea_mcp (0094), elitea_identity and
-- elitea_runtime. It is deliberately not `centry.`: that schema is the legacy
-- runtime's, and claiming an object in it is how a migration collides with a
-- shape someone else owns.
--
-- WHAT IS STORED, AND WHAT IS DELIBERATELY NOT
--
-- `token_hash`, NOT the token. The token is a bearer credential for conversation
-- content: anyone holding it reads the transcript with no further authentication.
-- Storing it in plaintext means a database dump, a log of a SELECT, or a
-- read-only replica compromise is immediately a set of working links against
-- every shared conversation in the deployment. So the column holds SHA-256 of
-- the token and the token itself exists exactly once, in the response to the
-- POST that created it.
--
-- SHA-256, unsalted and uniterated, is the right primitive HERE and would be
-- wrong for a password. The input is 256 bits from crypto/rand, so there is no
-- dictionary to run and no offline advantage to be had from iteration; what the
-- hash has to be is FAST, because it is computed on every anonymous view. The
-- password columns below are the opposite case and are treated as such.
--
-- `password_hash` + `password_salt` are PBKDF2-HMAC-SHA256 over a per-link
-- 128-bit random salt. That input IS user-chosen and low-entropy, so the
-- iteration count is the defence, and the salt is what stops one derived table
-- covering every link in the deployment. Both columns are NULL together for a
-- link with no password; the CHECK below makes "half a password" a state the
-- table cannot hold, so no reader has to decide what a salt with no hash means.
--
-- No copy of the conversation, its messages, or its participants is stored. The
-- link is a POINTER plus a scope; the view reads the live conversation through
-- the project id in this row. That is what makes deleting a message actually
-- un-share it, rather than leaving a frozen copy readable by a link nobody
-- remembers issuing.
--
-- REVOCATION AND EXPIRY ARE BOTH ROWS, NOT ABSENCES
--
-- `revoked_at` is a timestamp rather than a DELETE so that a revoked link
-- remains distinguishable from one that never existed FOR THE OWNER (the
-- listing can show it), while the anonymous view answers both with the same
-- 404 — see the handler's doc comment for why that symmetry is load-bearing.
--
-- `expires_at` is NOT NULL. A never-expiring link is a credential with no end
-- of life, handed to an audience the issuer stops thinking about within a day;
-- the API caps it instead, and the column's NOT NULL is what makes "no expiry"
-- unrepresentable rather than merely un-offered by the current UI.
--
-- Idempotent statements throughout, as the rest of this corpus is: dev and
-- dump-loaded databases reach this file in several different states. No
-- BEGIN/COMMIT — the ledgered runner wraps each file in one transaction with
-- its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_chat;

CREATE TABLE IF NOT EXISTS elitea_chat.shared_chat_links (
    id bigserial PRIMARY KEY,

    -- SHA-256 of the bearer token. UNIQUE because it is the lookup key of the
    -- anonymous read: a duplicate would make "which link is this" ambiguous at
    -- exactly the point where there is no other identity to fall back on.
    token_hash bytea NOT NULL UNIQUE,

    -- Server-derived tenant routing for the anonymous read. bigint, matching
    -- centry.project.id, because the reader turns it into `p_<id>` itself.
    project_id bigint NOT NULL,
    conversation_id bigint NOT NULL,

    -- 'all' shares the whole transcript; 'partial' shares only the message
    -- groups named in message_group_ids. The CHECK keeps a third value out
    -- rather than letting a reader guess what an unknown scope should show —
    -- and a reader that guessed "show everything" would be the worst possible
    -- default here.
    scope text NOT NULL DEFAULT 'all',
    message_group_ids bigint[] NOT NULL DEFAULT '{}',

    password_hash bytea,
    password_salt bytea,

    -- Who published it, as the string principal id the API layer uses. Kept so
    -- that an operator answering "who exposed this conversation" has an answer
    -- that does not depend on the issuer still being a project member.
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,

    -- Accounting the owner can see. It is the only signal a link-holder's
    -- activity leaves, and it is what makes "this got passed around further
    -- than I meant" observable at all.
    access_count bigint NOT NULL DEFAULT 0,
    last_accessed_at timestamptz,

    CONSTRAINT shared_chat_links_scope_check
        CHECK (scope IN ('all', 'partial')),
    -- Both password columns or neither. See the header: this makes a
    -- half-written password a state no reader has to interpret.
    CONSTRAINT shared_chat_links_password_pair_check
        CHECK ((password_hash IS NULL) = (password_salt IS NULL))
);

-- The owner-facing listing is always "the links on THIS conversation, newest
-- first". Without this index it is a sequential scan over every link in the
-- deployment, which is the one access path that grows with total usage rather
-- than with the caller's own data.
CREATE INDEX IF NOT EXISTS shared_chat_links_conversation_idx
    ON elitea_chat.shared_chat_links (project_id, conversation_id, created_at DESC);

-- Expiry sweeps read this. Partial on the live rows only: an expired or revoked
-- link is never the answer to anything, so it does not belong in the index that
-- finds work to do.
CREATE INDEX IF NOT EXISTS shared_chat_links_expiry_idx
    ON elitea_chat.shared_chat_links (expires_at)
    WHERE revoked_at IS NULL;

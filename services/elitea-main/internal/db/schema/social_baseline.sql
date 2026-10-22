-- SQLC compiler input for the current-baseline centry.social_users table.
--
-- This file is NOT a runtime migration. It projects the existing populated
-- legacy-era schema so generated queries remain compile-time checked while the
-- Social API is moved to elitea-main.

CREATE TABLE centry.social_users (
    id serial PRIMARY KEY,
    user_id integer NOT NULL UNIQUE,
    avatar varchar,
    title varchar,
    description varchar,
    personalization jsonb,
    default_context_management jsonb,
    default_summarization jsonb
);

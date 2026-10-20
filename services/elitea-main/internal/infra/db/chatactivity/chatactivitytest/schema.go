// Package chatactivitytest gives hand-cut test fixtures the columns and the
// objects the conversation activity reads (client contract 1.12) need.
// Fixtures that apply the real tenant migrations apply tenant/0153 instead.
package chatactivitytest

import "fmt"

// Schema is SQL that adds, to the tenant schema s (a trusted identifier),
// the message group columns chatactivity.Load and chatactivity.Paused read,
// the read marker table and the read baseline function. It keeps the
// columns and tables that the fixture already declared. The baseline is
// the start of time: every answer of a fixture is unread until it is read.
func Schema(s string) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s.chat_message_group (id serial PRIMARY KEY, conversation_id integer);
ALTER TABLE %[1]s.chat_message_group
    ADD COLUMN IF NOT EXISTS created_at timestamp DEFAULT (now() AT TIME ZONE 'UTC'),
    ADD COLUMN IF NOT EXISTS updated_at timestamp,
    ADD COLUMN IF NOT EXISTS is_streaming boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS meta jsonb DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS author_participant_id integer;
CREATE TABLE IF NOT EXISTS %[1]s.chat_participants (id serial PRIMARY KEY, entity_name text, entity_meta jsonb);
CREATE TABLE IF NOT EXISTS %[1]s.chat_conversation_reads (
    conversation_id integer NOT NULL REFERENCES %[1]s.chat_conversations(id) ON DELETE CASCADE,
    user_id bigint NOT NULL,
    read_at timestamp NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
    last_read_message_id integer,
    PRIMARY KEY (conversation_id, user_id));
CREATE OR REPLACE FUNCTION %[1]s.chat_conversation_read_baseline() RETURNS timestamp
    LANGUAGE sql STABLE AS $$ SELECT '-infinity'::timestamp $$;
`, s)
}

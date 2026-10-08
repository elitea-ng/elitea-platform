import type { Db } from '@/db/sql';

/**
 * Versioned schema (`PRAGMA user_version`). Each step runs in its own
 * transaction together with the version bump, so a crash leaves the database
 * at the previous version and the step runs again on the next open.
 *
 * Tables (every row belongs to one project; the whole file belongs to one
 * workspace origin, recorded in `meta`):
 * - `conversations`: list rows (`row_json`, the server's own shape) plus the
 *   last full read of the conversation (`detail_json`, participants included)
 *   so a thread can be shown and addressed offline;
 * - `messages`: transcript rows (message groups), keyed by group id;
 * - `notifications`;
 * - `sync_state`: one `changes_since` cursor per synced list (scope), with
 *   the generation counters a full resync sweeps by;
 * - `meta`: key/value (origin);
 * - `outbox` (2): sends waiting for the workspace (src/sync/outbox.ts);
 * - `conversation_search`, `message_search` (3): the offline chat search, FTS5
 *   tables INSIDE this encrypted file (no plaintext side table or file),
 *   kept in step with `conversations` and `messages` by triggers, so every
 *   writer (sync, retention, a tombstone, a dropped project) maintains the
 *   index without knowing about it (src/db/search.ts);
 * - `conversation_ops` (3): list actions (rename, pin, delete) waiting for
 *   the workspace (src/sync/conversationOps.ts).
 *
 * `messages` has no foreign key to `conversations`: a transcript may be
 * synced for a conversation the list filter does not include (a shared one).
 * Deleting a conversation deletes its messages explicitly.
 */

export interface Migration {
  version: number;
  sql: string;
}

export const MIGRATIONS: readonly Migration[] = [
  {
    version: 1,
    sql: `
      CREATE TABLE meta (
        key TEXT PRIMARY KEY NOT NULL,
        value TEXT NOT NULL
      );
      CREATE TABLE conversations (
        project_id INTEGER NOT NULL,
        id INTEGER NOT NULL,
        uuid TEXT,
        name TEXT NOT NULL DEFAULT '',
        updated_ms INTEGER,
        row_json TEXT,
        detail_json TEXT,
        opened_ms INTEGER,
        messages_dirty INTEGER NOT NULL DEFAULT 1,
        sync_gen INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (project_id, id)
      );
      CREATE INDEX conversations_by_update ON conversations (project_id, updated_ms DESC);
      CREATE INDEX conversations_by_uuid ON conversations (project_id, uuid);
      CREATE TABLE messages (
        project_id INTEGER NOT NULL,
        conversation_id INTEGER NOT NULL,
        id INTEGER NOT NULL,
        uid TEXT,
        created_ms INTEGER,
        row_json TEXT NOT NULL,
        sync_gen INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (project_id, conversation_id, id)
      );
      CREATE TABLE notifications (
        project_id INTEGER NOT NULL,
        id INTEGER NOT NULL,
        uuid TEXT,
        is_seen INTEGER NOT NULL DEFAULT 0,
        created_ms INTEGER,
        row_json TEXT NOT NULL,
        sync_gen INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (project_id, id)
      );
      CREATE INDEX notifications_by_created ON notifications (project_id, created_ms DESC);
      CREATE TABLE sync_state (
        scope TEXT PRIMARY KEY NOT NULL,
        project_id INTEGER NOT NULL,
        cursor TEXT,
        gen INTEGER NOT NULL DEFAULT 0,
        full_gen INTEGER,
        synced_ms INTEGER
      );
    `,
  },
  {
    // The outbox: sends made offline, each with its client question_id and
    // the EXACT body bytes, so every retry is the same admission.
    version: 2,
    sql: `
      CREATE TABLE outbox (
        question_id TEXT PRIMARY KEY NOT NULL,
        project_id INTEGER NOT NULL,
        conversation_ref TEXT NOT NULL,
        conversation_uuid TEXT NOT NULL,
        body_json TEXT NOT NULL,
        user_text TEXT NOT NULL,
        state TEXT NOT NULL DEFAULT 'queued',
        attempts INTEGER NOT NULL DEFAULT 0,
        next_attempt_ms INTEGER NOT NULL DEFAULT 0,
        last_error TEXT,
        created_ms INTEGER NOT NULL
      );
      CREATE INDEX outbox_by_conversation ON outbox (project_id, conversation_uuid, created_ms);
    `,
  },
  {
    // Offline search and the list actions. The FTS rowid IS the row's rowid
    // in its table: the triggers delete by it, so removing a row (any
    // writer, any WHERE) removes its index entry. Those rowids are stable:
    // the store never runs a full VACUUM (only `incremental_vacuum`, which
    // moves no rows). The message text is the row's `content` (the text the
    // thread shows; attachments are items, not text).
    version: 3,
    sql: `
      CREATE VIRTUAL TABLE conversation_search USING fts5(
        name, tokenize = 'unicode61 remove_diacritics 2', prefix = '2 3'
      );
      CREATE VIRTUAL TABLE message_search USING fts5(
        body, tokenize = 'unicode61 remove_diacritics 2', prefix = '2 3'
      );
      -- secure-delete: removing a row erases its tokens from the index b-tree at once,
      -- instead of leaving them behind a delete marker until a merge happens to rewrite
      -- that segment (a retained-out or deleted chat's words must not stay recoverable
      -- by whoever holds the key). Needs SQLite 3.42+ (the SQLCipher build is 3.53).
      INSERT INTO conversation_search (conversation_search, rank) VALUES ('secure-delete', 1);
      INSERT INTO message_search (message_search, rank) VALUES ('secure-delete', 1);
      CREATE TRIGGER conversation_search_insert AFTER INSERT ON conversations BEGIN
        INSERT INTO conversation_search (rowid, name) VALUES (new.rowid, new.name);
      END;
      CREATE TRIGGER conversation_search_update AFTER UPDATE OF name ON conversations BEGIN
        DELETE FROM conversation_search WHERE rowid = old.rowid;
        INSERT INTO conversation_search (rowid, name) VALUES (new.rowid, new.name);
      END;
      CREATE TRIGGER conversation_search_delete AFTER DELETE ON conversations BEGIN
        DELETE FROM conversation_search WHERE rowid = old.rowid;
      END;
      CREATE TRIGGER message_search_insert AFTER INSERT ON messages BEGIN
        INSERT INTO message_search (rowid, body) VALUES (
          new.rowid,
          CASE WHEN json_valid(new.row_json) AND json_type(new.row_json, '$.content') = 'text'
            THEN json_extract(new.row_json, '$.content') ELSE '' END
        );
      END;
      CREATE TRIGGER message_search_update AFTER UPDATE OF row_json ON messages BEGIN
        DELETE FROM message_search WHERE rowid = old.rowid;
        INSERT INTO message_search (rowid, body) VALUES (
          new.rowid,
          CASE WHEN json_valid(new.row_json) AND json_type(new.row_json, '$.content') = 'text'
            THEN json_extract(new.row_json, '$.content') ELSE '' END
        );
      END;
      CREATE TRIGGER message_search_delete AFTER DELETE ON messages BEGIN
        DELETE FROM message_search WHERE rowid = old.rowid;
      END;
      INSERT INTO conversation_search (rowid, name) SELECT rowid, name FROM conversations;
      INSERT INTO message_search (rowid, body)
        SELECT rowid, CASE WHEN json_valid(row_json) AND json_type(row_json, '$.content') = 'text'
          THEN json_extract(row_json, '$.content') ELSE '' END
        FROM messages;
      CREATE TABLE conversation_ops (
        project_id INTEGER NOT NULL,
        conversation_id INTEGER NOT NULL,
        kind TEXT NOT NULL,
        value TEXT,
        attempts INTEGER NOT NULL DEFAULT 0,
        next_attempt_ms INTEGER NOT NULL DEFAULT 0,
        created_ms INTEGER NOT NULL,
        PRIMARY KEY (project_id, conversation_id, kind)
      );
    `,
  },
];

export class FutureSchemaError extends Error {
  constructor(readonly found: number, readonly known: number) {
    super(`offline database schema ${found} is newer than this app (${known})`);
    this.name = 'FutureSchemaError';
  }
}

export const SCHEMA_VERSION = MIGRATIONS[MIGRATIONS.length - 1].version;

export async function schemaVersion(db: Db): Promise<number> {
  const row = await db.get<{ user_version: number }>('PRAGMA user_version;');
  return typeof row?.user_version === 'number' ? row.user_version : 0;
}

/**
 * Brings the schema up to `SCHEMA_VERSION`. Running it again is a no-op. A
 * file from a newer app (a downgrade) throws `FutureSchemaError`; the store
 * then discards the file (it is a cache) instead of guessing.
 */
export async function migrate(db: Db, migrations: readonly Migration[] = MIGRATIONS): Promise<number> {
  const known = migrations[migrations.length - 1].version;
  let current = await schemaVersion(db);
  if (current > known) throw new FutureSchemaError(current, known);
  if (current === 0) {
    // Must precede the first table: free pages are then returned to the file
    // system by `PRAGMA incremental_vacuum` (retention, size cap).
    await db.exec('PRAGMA auto_vacuum = INCREMENTAL;');
  }
  for (const step of migrations) {
    if (step.version <= current) continue;
    if (!Number.isSafeInteger(step.version) || step.version <= 0) throw new Error('bad migration version');
    await db.transaction(async (tx) => {
      await tx.exec(step.sql);
      await tx.exec(`PRAGMA user_version = ${step.version};`);
    });
    current = step.version;
  }
  return current;
}

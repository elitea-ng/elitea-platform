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
 * - `outbox` (2): sends waiting for the workspace (src/sync/outbox.ts).
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

import { FutureSchemaError, MIGRATIONS, SCHEMA_VERSION, migrate, schemaVersion } from '@/db/migrations';
import { Db } from '@/db/sql';
import { openNodeSqlite } from '@/test/nodeSqlite';

const tables = async (db: Db) =>
  (await db.all<{ name: string }>("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")).map((r) => r.name);

describe('migrate', () => {
  it('builds the schema from an empty file', async () => {
    const db = new Db(openNodeSqlite());
    expect(await migrate(db)).toBe(SCHEMA_VERSION);
    expect(await schemaVersion(db)).toBe(SCHEMA_VERSION);
    expect(await tables(db)).toEqual(expect.arrayContaining(['conversations', 'messages', 'meta', 'notifications', 'sync_state']));
    expect(await db.get('PRAGMA auto_vacuum;')).toEqual({ auto_vacuum: 2 }); // INCREMENTAL
  });

  it('upgrades a schema-1 file (before the outbox) and keeps its rows', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db, MIGRATIONS.slice(0, 1));
    await db.run("INSERT INTO conversations (project_id, id, row_json) VALUES (7, 1, '{}')");
    expect(await tables(db)).not.toContain('outbox');
    expect(await migrate(db)).toBe(SCHEMA_VERSION);
    expect(await tables(db)).toContain('outbox');
    expect(await db.get('SELECT count(*) AS n FROM conversations')).toEqual({ n: 1 });
  });

  it('is a no-op the second time', async () => {
    const raw = openNodeSqlite();
    const db = new Db(raw);
    await migrate(db);
    const before = raw.statements.length;
    await migrate(db);
    expect(raw.statements.slice(before)).toEqual(['PRAGMA user_version;']);
  });

  it('runs only the steps above the stored version, each with its version bump', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db, MIGRATIONS);
    const next = [...MIGRATIONS, { version: SCHEMA_VERSION + 1, sql: 'CREATE TABLE extra (x INTEGER);' }];
    expect(await migrate(db, next)).toBe(SCHEMA_VERSION + 1);
    expect(await tables(db)).toContain('extra');
  });

  it('leaves the previous version when a step fails', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    const broken = [...MIGRATIONS, { version: SCHEMA_VERSION + 1, sql: 'CREATE TABLE ok (x); CREATE TABLE broken (' }];
    await expect(migrate(db, broken)).rejects.toThrow();
    expect(await schemaVersion(db)).toBe(SCHEMA_VERSION);
    expect(await tables(db)).not.toContain('ok');
  });

  it('refuses a file written by a newer app', async () => {
    const db = new Db(openNodeSqlite());
    await db.exec(`PRAGMA user_version = ${SCHEMA_VERSION + 5};`);
    await expect(migrate(db)).rejects.toBeInstanceOf(FutureSchemaError);
  });
});

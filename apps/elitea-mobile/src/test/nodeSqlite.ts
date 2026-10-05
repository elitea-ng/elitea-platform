/**
 * jest-only SQLite driver over node's built-in `node:sqlite` (no native
 * dependency), implementing the same `RawConnection` the app runs over
 * expo-sqlite. It has no SQLCipher, so stores built on it pass
 * `requireCipher: false`; the cipher handshake itself is tested with a
 * scripted connection (src/db/open.test.ts).
 */
import type { RawConnection, SqlValue } from '@/db/sql';

interface StatementSync {
  run(...params: SqlValue[]): { changes: number | bigint };
  all(...params: SqlValue[]): unknown[];
  get(...params: SqlValue[]): unknown;
}
interface DatabaseSync {
  exec(sql: string): void;
  prepare(sql: string): StatementSync;
  close(): void;
  readonly isOpen: boolean;
}

const { DatabaseSync: NodeDatabase } = require('node:sqlite') as {
  DatabaseSync: new (path: string) => DatabaseSync;
};

export interface NodeSqliteConnection extends RawConnection {
  /** Every statement in order (exec and prepared), for assertions. */
  statements: string[];
  readonly isOpen: boolean;
}

export function openNodeSqlite(path = ':memory:'): NodeSqliteConnection {
  const db = new NodeDatabase(path);
  const statements: string[] = [];
  const conn: NodeSqliteConnection = {
    statements,
    get isOpen() {
      return db.isOpen;
    },
    exec: async (sql) => {
      statements.push(sql);
      db.exec(sql);
    },
    run: async (sql, params = []) => {
      statements.push(sql);
      const result = db.prepare(sql).run(...params);
      return { changes: Number(result.changes) };
    },
    all: async <T>(sql: string, params: readonly SqlValue[] = []) => {
      statements.push(sql);
      return db.prepare(sql).all(...params) as T[];
    },
    get: async <T>(sql: string, params: readonly SqlValue[] = []) => {
      statements.push(sql);
      return (db.prepare(sql).get(...params) as T | undefined) ?? null;
    },
    close: async () => {
      if (db.isOpen) db.close();
    },
  };
  return conn;
}

/**
 * A file-backed driver for OfflineStore tests: files live in a temp dir and
 * `remove` deletes them (refusing while a connection is open, like
 * expo-sqlite). With `cipher: true` it simulates SQLCipher on top of plain
 * SQLite: `PRAGMA cipher_version` answers, and a file remembers the key it was
 * first keyed with, so opening it with another key fails like a wrong key
 * ("file is not a database") until the file is removed.
 */
export function nodeSqliteDriver(dir: string, options: { cipher?: boolean } = {}) {
  const { existsSync, rmSync } = require('node:fs') as {
    existsSync(path: string): boolean;
    rmSync(path: string, options: { force: boolean }): void;
  };
  const { join } = require('node:path') as { join(...parts: string[]): string };
  const opened: NodeSqliteConnection[] = [];
  const fileKeys = new Map<string, string>();
  const cipher = options.cipher === true;

  const withCipher = (name: string, conn: NodeSqliteConnection): NodeSqliteConnection => {
    let key: string | null = null;
    const wrongKey = () => key === null || (fileKeys.has(name) && fileKeys.get(name) !== key);
    return Object.assign(Object.create(conn) as NodeSqliteConnection, {
      exec: async (sql: string) => {
        const match = /^PRAGMA key = "x'([0-9a-f]+)'";$/.exec(sql);
        if (match) {
          conn.statements.push(sql);
          key = match[1];
          return;
        }
        if (sql.startsWith('PRAGMA cipher_memory_security')) {
          conn.statements.push(sql);
          return;
        }
        if (wrongKey()) throw new Error('file is not a database');
        if (!fileKeys.has(name) && key !== null) fileKeys.set(name, key);
        return conn.exec(sql);
      },
      get: async <T>(sql: string, params?: readonly SqlValue[]) => {
        if (sql.startsWith('PRAGMA cipher_version')) {
          conn.statements.push(sql);
          return { cipher_version: '4.6.1 community' } as T;
        }
        if (wrongKey()) throw new Error('file is not a database');
        return conn.get<T>(sql, params);
      },
    });
  };

  return {
    requireCipher: cipher,
    opened,
    fileKeys,
    exists: (name: string) => existsSync(join(dir, name)),
    open: async (name: string): Promise<NodeSqliteConnection> => {
      const conn = openNodeSqlite(join(dir, name));
      opened.push(conn);
      return cipher ? withCipher(name, conn) : conn;
    },
    remove: jest.fn(async (name: string) => {
      if (opened.some((c) => c.isOpen)) throw new Error('database is open');
      fileKeys.delete(name);
      for (const suffix of ['', '-journal', '-wal', '-shm']) rmSync(join(dir, name + suffix), { force: true });
    }),
  };
}

/** A fresh temp directory for one test file. */
export function tempDir(prefix = 'az-db-'): string {
  const { mkdtempSync } = require('node:fs') as { mkdtempSync(prefix: string): string };
  const { tmpdir } = require('node:os') as { tmpdir(): string };
  const { join } = require('node:path') as { join(...parts: string[]): string };
  return mkdtempSync(join(tmpdir(), prefix));
}

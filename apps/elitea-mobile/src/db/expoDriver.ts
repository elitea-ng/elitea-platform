import { deleteDatabaseAsync, openDatabaseAsync, type SQLiteBindValue } from 'expo-sqlite';

import type { RawConnection, SqlValue } from '@/db/sql';

/**
 * The only use of `expo-sqlite`. The native build links SQLCipher
 * (`useSQLCipher` in app.config.ts), so the file is encrypted once the
 * connection's first statement sets the key (src/db/open.ts).
 *
 * Only the async, single-connection API is used: `withExclusiveTransactionAsync`
 * would open a second connection without the key.
 */

const bind = (params: readonly SqlValue[] | undefined): SQLiteBindValue[] => (params ? [...params] : []);

export async function openExpoConnection(name: string): Promise<RawConnection> {
  // A new connection every time: a cached one could have been opened with another key.
  const db = await openDatabaseAsync(name, { useNewConnection: true });
  return {
    exec: (sql) => db.execAsync(sql),
    run: async (sql, params) => {
      const result = await db.runAsync(sql, bind(params));
      return { changes: result.changes };
    },
    all: (sql, params) => db.getAllAsync(sql, bind(params)),
    get: async (sql, params) => (await db.getFirstAsync(sql, bind(params))) ?? null,
    close: () => db.closeAsync(),
  };
}

/**
 * Deletes the database file and its -journal/-wal/-shm sidecars. The
 * connection must be closed first (expo-sqlite refuses to delete an open
 * database). A file that does not exist counts as deleted.
 */
export async function deleteExpoDatabase(name: string): Promise<void> {
  try {
    await deleteDatabaseAsync(name);
  } catch (error) {
    if (isNotFound(error)) return;
    throw error;
  }
}

function isNotFound(error: unknown): boolean {
  const text = `${(error as { code?: unknown } | null)?.code ?? ''} ${(error as Error | null)?.message ?? ''}`;
  return /not.?found|does not exist/i.test(text);
}

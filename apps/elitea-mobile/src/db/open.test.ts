import { DbOpenError, keyConnection } from '@/db/open';
import type { RawConnection } from '@/db/sql';

const KEY = 'a'.repeat(64);

function scripted(answers: { cipherVersion?: string | null; masterFails?: boolean } = {}) {
  const statements: string[] = [];
  const raw: RawConnection = {
    exec: async (sql) => {
      statements.push(sql);
    },
    run: async (sql) => {
      statements.push(sql);
      return { changes: 0 };
    },
    all: async (sql) => {
      statements.push(sql);
      return [];
    },
    get: async <T,>(sql: string) => {
      statements.push(sql);
      if (sql.startsWith('PRAGMA cipher_version')) {
        return (answers.cipherVersion === null ? null : { cipher_version: answers.cipherVersion ?? '4.6.1 community' }) as T | null;
      }
      if (sql.includes('sqlite_master') && answers.masterFails) throw new Error('file is not a database');
      return { n: 0 } as T;
    },
    close: async () => {},
  };
  return { raw, statements };
}

describe('keyConnection', () => {
  it('sets the key as the very first statement, then checks the cipher and the key', async () => {
    const { raw, statements } = scripted();
    await keyConnection(raw, KEY, { requireCipher: true });
    expect(statements[0]).toBe(`PRAGMA key = "x'${KEY}'";`);
    expect(statements).toEqual([
      `PRAGMA key = "x'${KEY}'";`,
      'PRAGMA cipher_version;',
      'PRAGMA cipher_memory_security = ON;',
      'SELECT count(*) AS n FROM sqlite_master;',
      'PRAGMA journal_mode = WAL;',
      // Freed pages are zeroed: a deleted message's bytes do not linger in free space.
      'PRAGMA secure_delete = ON;',
    ]);
  });

  it('fails closed on a binary without SQLCipher', async () => {
    for (const cipherVersion of [null, '']) {
      const { raw, statements } = scripted({ cipherVersion });
      await expect(keyConnection(raw, KEY, { requireCipher: true })).rejects.toMatchObject({ reason: 'no_cipher' });
      expect(statements.some((s) => s.includes('sqlite_master'))).toBe(false);
    }
  });

  it('fails closed on a wrong key', async () => {
    const { raw } = scripted({ masterFails: true });
    const error = await keyConnection(raw, KEY, { requireCipher: true }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(DbOpenError);
    expect((error as DbOpenError).reason).toBe('wrong_key');
  });

  it('never interpolates anything but 64 hex characters', async () => {
    for (const bad of ['', 'A'.repeat(64), `${'a'.repeat(63)}"`, "x'00'; ATTACH"]) {
      const { raw, statements } = scripted();
      await expect(keyConnection(raw, bad, { requireCipher: true })).rejects.toMatchObject({ reason: 'bad_key' });
      expect(statements).toEqual([]);
    }
  });
});

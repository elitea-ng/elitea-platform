import { DB_KEY_RE } from '@/db/key';
import type { RawConnection } from '@/db/sql';

/**
 * Keys a fresh connection. The key must be the connection's FIRST statement:
 * SQLCipher derives the page key from it before anything else reads the file.
 *
 * Fails closed:
 * - `no_cipher`: the binary was built without SQLCipher (`PRAGMA
 *   cipher_version` is empty). Writing would create a plaintext file, so the
 *   store refuses to run at all;
 * - `wrong_key`: the file does not decrypt with this key (or is not a
 *   database). Nothing is read; the caller deletes the file and starts over.
 */

export type OpenFailure = 'no_cipher' | 'wrong_key' | 'bad_key';

export class DbOpenError extends Error {
  constructor(readonly reason: OpenFailure, cause?: unknown) {
    super(`offline database could not be opened: ${reason}`, cause === undefined ? undefined : { cause });
    this.name = 'DbOpenError';
  }
}

export async function keyConnection(
  raw: RawConnection,
  keyHex: string,
  options: { requireCipher: boolean },
): Promise<void> {
  // The key is interpolated (PRAGMA takes no bound parameters), so only the
  // exact 64-hex form may ever reach the statement.
  if (!DB_KEY_RE.test(keyHex)) throw new DbOpenError('bad_key');
  await raw.exec(`PRAGMA key = "x'${keyHex}'";`);
  if (options.requireCipher) {
    let version: string | null = null;
    try {
      const row = await raw.get<{ cipher_version?: unknown }>('PRAGMA cipher_version;');
      version = typeof row?.cipher_version === 'string' ? row.cipher_version : null;
    } catch {
      version = null;
    }
    if (!version) throw new DbOpenError('no_cipher');
    await raw.exec('PRAGMA cipher_memory_security = ON;');
  }
  try {
    await raw.get('SELECT count(*) AS n FROM sqlite_master;');
  } catch (error) {
    throw new DbOpenError('wrong_key', error);
  }
  await raw.exec('PRAGMA journal_mode = WAL;');
}

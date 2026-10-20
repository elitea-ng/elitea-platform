import { getRandomBytes } from 'expo-crypto';

import { SECURE_KEYS, deleteSecureItemIn, getSecureItemIn, setSecureItemIn } from '@/security/secureStorage';

/**
 * The offline database key: 32 random bytes, hex encoded, kept in the
 * Keychain / Keystore (`az.db.key` in the workspace's namespace,
 * `WHEN_UNLOCKED_THIS_DEVICE_ONLY`, wiped with every other secure item of the
 * workspace by endSession). Each workspace has its own key and file.
 *
 * Release rule: nothing reads the key until the device-lock gate is open
 * (src/db/offlineBinding.ts opens the store only while the lock controller
 * says `unlocked`, which under `require_device_lock` means the user passed
 * biometrics or the device passcode). Locking closes the database and drops
 * the in-memory key with it.
 */

export const DB_KEY_RE = /^[0-9a-f]{64}$/;

export function newDbKey(random: (count: number) => Uint8Array = getRandomBytes): string {
  const bytes = random(32);
  if (bytes.length !== 32) throw new Error('random source returned the wrong number of bytes');
  let hex = '';
  for (const byte of bytes) hex += byte.toString(16).padStart(2, '0');
  if (!DB_KEY_RE.test(hex)) throw new Error('database key is malformed');
  return hex;
}

/** The stored key; null when absent or malformed (either way the old file cannot be opened). */
export async function readDbKey(workspaceId: string): Promise<string | null> {
  const value = await getSecureItemIn(workspaceId, SECURE_KEYS.dbKey);
  return value !== null && DB_KEY_RE.test(value) ? value : null;
}

/** Creates and stores a new key, replacing any old one. */
export async function createDbKey(workspaceId: string, random?: (count: number) => Uint8Array): Promise<string> {
  const key = newDbKey(random);
  await setSecureItemIn(workspaceId, SECURE_KEYS.dbKey, key);
  return key;
}

export async function deleteDbKey(workspaceId: string): Promise<void> {
  await deleteSecureItemIn(workspaceId, SECURE_KEYS.dbKey);
}

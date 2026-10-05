import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

/**
 * The ONLY module allowed to import `expo-secure-store` (enforced by ESLint).
 *
 * Every key the app ever stores is declared in `SECURE_KEYS`. The logout-and-wipe
 * orchestrator (`endSession`) iterates this registry, so a key declared here is
 * wiped automatically and a key that is not declared here cannot be written.
 *
 * Every write uses `WHEN_UNLOCKED_THIS_DEVICE_ONLY`: the item is readable only
 * while the device is unlocked and is never migrated to another device through
 * a backup.
 */
export const SECURE_KEYS = {
  /** Origin of the connected workspace server, e.g. `https://ai.example.com`. */
  serverOrigin: 'az.server.origin',
  /** Kind of the stored credential: `pat` (developer sign-in) or `oauth` (B4). */
  credentialKind: 'az.auth.kind',
  /** Developer sign-in personal access token. */
  pat: 'az.auth.pat',
  /** Native sign-in access token (B4). Declared now so every wipe already covers it. */
  accessToken: 'az.auth.access',
  /** Native sign-in refresh token (B4). */
  refreshToken: 'az.auth.refresh',
  /** The selected project id. */
  projectSelected: 'az.project.selected',
  /** Last server-supplied client policy (B4). */
  policy: 'az.policy',
  /** The connected workspace's mapped brand theme (non-secret; wiped with the workspace). */
  brandTheme: 'az.brand.theme',
  /** Key of the encrypted local database (B4). */
  dbKey: 'az.db.key',
  /** Crash-resume marker written first and removed last by `endSession`. */
  wipePending: 'az.wipe.pending',
} as const;

export type SecureKey = (typeof SECURE_KEYS)[keyof typeof SECURE_KEYS];

export const REGISTERED_KEYS: readonly SecureKey[] = Object.freeze(Object.values(SECURE_KEYS));

const WRITE_OPTIONS: SecureStore.SecureStoreOptions = {
  keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
};

export class UnregisteredSecureKeyError extends Error {
  constructor(key: string) {
    super(`secure storage key "${key}" is not declared in SECURE_KEYS`);
    this.name = 'UnregisteredSecureKeyError';
  }
}

function assertRegistered(key: string): asserts key is SecureKey {
  if (!(REGISTERED_KEYS as readonly string[]).includes(key)) {
    throw new UnregisteredSecureKeyError(key);
  }
}

/**
 * Backend selection. Native uses the Keychain / Keystore. The web build is a
 * development surface only and has no secure storage: it keeps values in
 * memory for the lifetime of the page and never persists them.
 */
interface Backend {
  get(key: string): Promise<string | null>;
  set(key: string, value: string): Promise<void>;
  delete(key: string): Promise<void>;
}

const nativeBackend: Backend = {
  get: (key) => SecureStore.getItemAsync(key, WRITE_OPTIONS),
  set: (key, value) => SecureStore.setItemAsync(key, value, WRITE_OPTIONS),
  delete: (key) => SecureStore.deleteItemAsync(key, WRITE_OPTIONS),
};

function memoryBackend(): Backend {
  const values = new Map<string, string>();
  return {
    get: async (key) => values.get(key) ?? null,
    set: async (key, value) => {
      values.set(key, value);
    },
    delete: async (key) => {
      values.delete(key);
    },
  };
}

const backend: Backend = Platform.OS === 'web' ? memoryBackend() : nativeBackend;

export async function getSecureItem(key: SecureKey): Promise<string | null> {
  assertRegistered(key);
  return backend.get(key);
}

export async function setSecureItem(key: SecureKey, value: string): Promise<void> {
  assertRegistered(key);
  await backend.set(key, value);
}

export async function deleteSecureItem(key: SecureKey): Promise<void> {
  assertRegistered(key);
  await backend.delete(key);
}

/**
 * Deletes every registered key except `keep`. Never throws: each deletion is
 * attempted, and the failures are returned so the caller can report them.
 */
export async function deleteAllSecureItems(
  keep: readonly SecureKey[] = [],
): Promise<{ key: SecureKey; error: unknown }[]> {
  const failures: { key: SecureKey; error: unknown }[] = [];
  for (const key of REGISTERED_KEYS) {
    if (keep.includes(key)) continue;
    try {
      await backend.delete(key);
    } catch (error) {
      failures.push({ key, error });
    }
  }
  return failures;
}

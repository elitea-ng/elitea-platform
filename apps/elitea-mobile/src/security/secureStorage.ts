import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

/**
 * The ONLY module allowed to import `expo-secure-store` (enforced by ESLint).
 *
 * Several workspaces can be connected at once, each fully isolated. Every
 * key the app stores for a workspace is declared in `SECURE_KEYS` (logical
 * names) and lives in that workspace's own namespace: the physical keychain
 * item is `az.ws.<workspace id>.<name>` (`physicalKey`). The logout-and-wipe
 * orchestrator (`endSession`) iterates the registry inside ONE namespace, so
 * a key declared here is wiped with its workspace and a key that is not
 * declared here cannot be written.
 *
 * `getSecureItem` / `setSecureItem` / `deleteSecureItem` act on the BOUND
 * workspace (`bindWorkspace`, the active one) and throw `NoWorkspaceError`
 * while none is bound (signed out, or in the middle of a switch). The
 * physical key is resolved synchronously when the call starts, so a write
 * that began for one workspace can never land in another one's namespace.
 * The `…In(workspaceId, …)` variants address a workspace explicitly (the
 * switcher list, removing a workspace that is not active).
 *
 * A few items belong to the device, not to a workspace (`DEVICE_KEYS`: the
 * workspace registry).
 *
 * Every write uses `WHEN_UNLOCKED_THIS_DEVICE_ONLY`: the item is readable only
 * while the device is unlocked and is never migrated to another device through
 * a backup.
 */
export const SECURE_KEYS = {
  /** Origin of the workspace server, e.g. `https://ai.example.com`. */
  serverOrigin: 'az.server.origin',
  /** Non-secret facts about the workspace slot: its database file name and when it was added. */
  workspaceMeta: 'az.workspace.meta',
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
  /** The workspace's mapped brand theme (non-secret; wiped with the workspace). */
  brandTheme: 'az.brand.theme',
  /** The signed-in user's id for the workspace ("who am I", contract 1.2; non-secret, wiped with the session). */
  selfUser: 'az.user.self',
  /** Key of the workspace's encrypted local database (B4). */
  dbKey: 'az.db.key',
  /** Crash-resume marker written first and removed last by `endSession`. */
  wipePending: 'az.wipe.pending',
} as const;

export type SecureKey = (typeof SECURE_KEYS)[keyof typeof SECURE_KEYS];

export const REGISTERED_KEYS: readonly SecureKey[] = Object.freeze(Object.values(SECURE_KEYS));

/** Device-wide items (not part of any workspace). */
export const DEVICE_KEYS = {
  /** Settings → Appearance: `system`, `light` or `dark` (a device convenience, not workspace data). */
  appearance: 'az.appearance',
  /** The workspace registry: `{v, active, ids}` (src/security/workspaceRegistry.ts). Purged last. */
  workspaces: 'az.workspaces',
} as const;

export type DeviceKey = (typeof DEVICE_KEYS)[keyof typeof DEVICE_KEYS];

const DEVICE_KEY_LIST: readonly DeviceKey[] = Object.freeze(Object.values(DEVICE_KEYS));

/** A workspace id: 12 lowercase hex characters (a valid keychain key segment). */
export const WORKSPACE_ID_RE = /^[0-9a-f]{12}$/;

/** The keychain item that holds `key` for `workspaceId`. */
export function physicalKey(workspaceId: string, key: SecureKey): string {
  if (!WORKSPACE_ID_RE.test(workspaceId)) throw new Error('invalid workspace id');
  return `az.ws.${workspaceId}.${key.slice('az.'.length)}`;
}

const WRITE_OPTIONS: SecureStore.SecureStoreOptions = {
  keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
};

export class UnregisteredSecureKeyError extends Error {
  constructor(key: string) {
    super(`secure storage key "${key}" is not declared in SECURE_KEYS`);
    this.name = 'UnregisteredSecureKeyError';
  }
}

/** A workspace item was used while no workspace is bound (signed out, or switching). */
export class NoWorkspaceError extends Error {
  constructor() {
    super('no workspace is bound to secure storage');
    this.name = 'NoWorkspaceError';
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

// --- the bound (active) workspace ------------------------------------------

let bound: string | null = null;

/** Makes `workspaceId` the namespace of the unqualified calls below (null: none). */
export function bindWorkspace(workspaceId: string | null): void {
  if (workspaceId !== null && !WORKSPACE_ID_RE.test(workspaceId)) throw new Error('invalid workspace id');
  bound = workspaceId;
}

/** The bound workspace id, or null. */
export function boundWorkspace(): string | null {
  return bound;
}

function requireBound(): string {
  if (bound === null) throw new NoWorkspaceError();
  return bound;
}

export async function getSecureItem(key: SecureKey): Promise<string | null> {
  assertRegistered(key);
  return getSecureItemIn(requireBound(), key);
}

export async function setSecureItem(key: SecureKey, value: string): Promise<void> {
  assertRegistered(key);
  await setSecureItemIn(requireBound(), key, value);
}

export async function deleteSecureItem(key: SecureKey): Promise<void> {
  assertRegistered(key);
  await deleteSecureItemIn(requireBound(), key);
}

// --- an explicit workspace --------------------------------------------------

export async function getSecureItemIn(workspaceId: string, key: SecureKey): Promise<string | null> {
  assertRegistered(key);
  return backend.get(physicalKey(workspaceId, key));
}

export async function setSecureItemIn(workspaceId: string, key: SecureKey, value: string): Promise<void> {
  assertRegistered(key);
  await backend.set(physicalKey(workspaceId, key), value);
}

export async function deleteSecureItemIn(workspaceId: string, key: SecureKey): Promise<void> {
  assertRegistered(key);
  await backend.delete(physicalKey(workspaceId, key));
}

export type DeleteFailure = { key: string; error: unknown };

/**
 * Deletes every registered key of `workspaceId` except `keep`. Never throws:
 * each deletion is attempted, and the failures are returned so the caller can
 * report them. Other workspaces are not touched.
 */
export async function deleteAllSecureItemsIn(
  workspaceId: string,
  keep: readonly SecureKey[] = [],
): Promise<DeleteFailure[]> {
  const failures: DeleteFailure[] = [];
  for (const key of REGISTERED_KEYS) {
    if (keep.includes(key)) continue;
    try {
      await backend.delete(physicalKey(workspaceId, key));
    } catch (error) {
      failures.push({ key, error });
    }
  }
  return failures;
}

// --- device items -----------------------------------------------------------

function assertDeviceKey(key: string): asserts key is DeviceKey {
  if (!(DEVICE_KEY_LIST as readonly string[]).includes(key)) throw new UnregisteredSecureKeyError(key);
}

export async function getDeviceItem(key: DeviceKey): Promise<string | null> {
  assertDeviceKey(key);
  return backend.get(key);
}

export async function setDeviceItem(key: DeviceKey, value: string): Promise<void> {
  assertDeviceKey(key);
  await backend.set(key, value);
}

export async function deleteDeviceItem(key: DeviceKey): Promise<void> {
  assertDeviceKey(key);
  await backend.delete(key);
}

// --- the single-workspace layout of earlier app versions ---------------------

/**
 * Before multi-workspace (app ≤ 0.1, contract 1.2 builds) every item was stored
 * under its logical name (`az.auth.pat`, …). Only the upgrade migration
 * (workspaceRegistry.ts) and the fresh-install purge read or delete those.
 */
export async function getLegacyItem(key: SecureKey): Promise<string | null> {
  assertRegistered(key);
  return backend.get(key);
}

/** Deletes every legacy (pre-workspace) item. Never throws; failures are returned. */
export async function deleteAllLegacyItems(): Promise<DeleteFailure[]> {
  const failures: DeleteFailure[] = [];
  for (const key of REGISTERED_KEYS) {
    try {
      await backend.delete(key);
    } catch (error) {
      failures.push({ key, error });
    }
  }
  return failures;
}

/** The workspace ids named by a stored registry value; [] when it is absent or unreadable. */
export function registryIds(raw: string | null): string[] {
  if (raw === null) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    const ids = (parsed as { ids?: unknown } | null)?.ids;
    if (!Array.isArray(ids)) return [];
    return ids.filter((id): id is string => typeof id === 'string' && WORKSPACE_ID_RE.test(id));
  } catch {
    return [];
  }
}

/**
 * Deletes EVERYTHING the app ever stored in secure storage on this device:
 * every workspace named by the registry, the legacy layout, then the device
 * items (the registry last, so an interrupted purge can still find the
 * workspaces next time). Never throws; failures are returned. Used by the
 * fresh-install purge.
 */
export async function deleteAllSecureItems(): Promise<DeleteFailure[]> {
  const failures: DeleteFailure[] = [];
  let ids: string[] = [];
  try {
    ids = registryIds(await backend.get(DEVICE_KEYS.workspaces));
  } catch (error) {
    failures.push({ key: DEVICE_KEYS.workspaces, error });
  }
  for (const id of ids) {
    for (const failure of await deleteAllSecureItemsIn(id)) {
      failures.push({ key: physicalKey(id, failure.key as SecureKey), error: failure.error });
    }
  }
  failures.push(...(await deleteAllLegacyItems()));
  // Keep the registry while anything it names may still be stored.
  if (failures.length === 0) {
    for (const key of DEVICE_KEY_LIST) {
      try {
        await backend.delete(key);
      } catch (error) {
        failures.push({ key, error });
      }
    }
  }
  return failures;
}

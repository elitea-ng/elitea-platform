import { getRandomBytes } from 'expo-crypto';
import { z } from 'zod';

import { log } from '@/lib/log';
import {
  DEVICE_KEYS,
  REGISTERED_KEYS,
  SECURE_KEYS,
  WORKSPACE_ID_RE,
  deleteAllLegacyItems,
  deleteAllSecureItemsIn,
  getDeviceItem,
  getLegacyItem,
  getSecureItemIn,
  setDeviceItem,
  setSecureItemIn,
} from '@/security/secureStorage';

/**
 * The connected workspaces of this device.
 *
 * Each workspace has its own slot: a random id that namespaces its secure
 * items (secureStorage.ts), its own encrypted database file and key, brand,
 * policy, outbox and sync cursors (all inside that database or namespace) and
 * its own device session. The registry itself is one small device item:
 *
 *   az.workspaces  {v: 1, active: <id> | null, ids: [<id>, …]}
 *
 * `ids` is most-recently-used first; `active` is the workspace the app opens
 * at launch (null: the connect screen, with the others listed). The origin of
 * a slot is its own `az.server.origin` item, so the registry holds no host.
 *
 * Mutations are serialised (one read-modify-write at a time) and the latest
 * registry is kept in memory for the switcher (`subscribeRegistry`).
 */

export const MAX_WORKSPACES = 8;
/** The database file of the single-workspace layout, kept by the migrated slot. */
export const LEGACY_DB_NAME = 'az-offline.db';
/** The slot the single-workspace layout is migrated into (deterministic, so a retried migration is idempotent). */
export const MIGRATED_WORKSPACE_ID = '000000000001';
export const DB_NAME_RE = /^az-offline(-[0-9a-f]{12})?\.db$/;

export function dbNameFor(workspaceId: string): string {
  if (!WORKSPACE_ID_RE.test(workspaceId)) throw new Error('invalid workspace id');
  return `az-offline-${workspaceId}.db`;
}

export interface Registry {
  v: 1;
  active: string | null;
  ids: string[];
}

export const EMPTY_REGISTRY: Readonly<Registry> = Object.freeze({ v: 1, active: null, ids: [] });

const Id = z.string().regex(WORKSPACE_ID_RE);
const RegistrySchema = z.object({ v: z.literal(1), active: Id.nullable(), ids: z.array(Id).max(64) });

const MetaSchema = z.object({
  v: z.literal(1),
  dbName: z.string().regex(DB_NAME_RE),
  addedAt: z.number().int().nonnegative(),
  /**
   * When this workspace's database was last open (opened, retention re-run,
   * closed): its file receives data only then. Absent in a slot written by
   * an older app version. Lets a put-aside workspace's retention be applied
   * without opening it (src/db/offlineStore.ts `expireIfUnused`, #45).
   */
  usedAt: z.number().int().nonnegative().optional(),
});
export type WorkspaceMeta = z.infer<typeof MetaSchema>;

export class TooManyWorkspacesError extends Error {
  constructor() {
    super(`at most ${MAX_WORKSPACES} workspaces can be connected`);
    this.name = 'TooManyWorkspacesError';
  }
}

/** `createWorkspaceSlot`: a slot for that origin is already registered (`id`). */
export class WorkspaceExistsError extends Error {
  constructor(readonly id: string) {
    super('a workspace for this origin is already connected');
    this.name = 'WorkspaceExistsError';
  }
}

// --- in-memory copy ----------------------------------------------------------

let snapshot: Registry = { ...EMPTY_REGISTRY, ids: [] };
const listeners = new Set<() => void>();

function same(a: Registry, b: Registry): boolean {
  return a.active === b.active && a.ids.length === b.ids.length && a.ids.every((id, i) => b.ids[i] === id);
}

/** Records `next`; listeners hear only about an actual change. */
function publish(next: Registry): void {
  if (same(snapshot, next)) return;
  snapshot = next;
  for (const listener of [...listeners]) listener();
}

/** The registry as last read or written. */
export function getRegistrySnapshot(): Registry {
  return snapshot;
}

export function subscribeRegistry(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

// --- storage -----------------------------------------------------------------

/** Normalises a parsed registry: unique ids, `active` one of them. */
function normalise(registry: Registry): Registry {
  const ids = [...new Set(registry.ids)];
  const active = registry.active !== null && ids.includes(registry.active) ? registry.active : null;
  return { v: 1, active, ids };
}

/**
 * Reads the registry. A keychain error rejects (the caller retries when the
 * device is unlocked); a missing or malformed value reads as empty.
 */
export async function readRegistry(): Promise<Registry> {
  const raw = await getDeviceItem(DEVICE_KEYS.workspaces);
  let next: Registry = { ...EMPTY_REGISTRY, ids: [] };
  if (raw !== null) {
    try {
      next = normalise(RegistrySchema.parse(JSON.parse(raw)));
    } catch {
      log.warn('workspace registry unreadable; treated as empty');
    }
  }
  publish(next);
  return next;
}

async function writeRegistry(next: Registry): Promise<Registry> {
  const value = normalise(next);
  await setDeviceItem(DEVICE_KEYS.workspaces, JSON.stringify(value));
  publish(value);
  return value;
}

let chain: Promise<unknown> = Promise.resolve();

/**
 * Read-modify-write, serialised. Rejects (nothing changed) when the read, the
 * change or the write fails. `change` may be async (checks that must see the
 * same registry it replaces).
 */
export function updateRegistry(change: (current: Registry) => Registry | Promise<Registry>): Promise<Registry> {
  const next = chain.then(async () => writeRegistry(await change(await readRegistry())));
  chain = next.catch(() => undefined);
  return next;
}

/** Makes `id` the active workspace and the most recently used one. */
export function activateInRegistry(id: string): Promise<Registry> {
  return updateRegistry((r) => ({ v: 1, active: id, ids: [id, ...r.ids.filter((x) => x !== id)] }));
}

/**
 * The first sign-in into slot `id` finished: it becomes the active (launch)
 * workspace. Until then a new slot is not active, so backing out of its
 * sign-in, or a relaunch on its sign-in screen, still opens the workspace that
 * was active before. A slot no longer registered (removed meanwhile) is not
 * added back. Best effort: never throws.
 */
export async function activateSignedInWorkspace(id: string): Promise<void> {
  try {
    const r = await readRegistry();
    if (!r.ids.includes(id) || r.active === id) return;
    await updateRegistry((cur) =>
      cur.ids.includes(id) ? { v: 1, active: id, ids: [id, ...cur.ids.filter((x) => x !== id)] } : cur,
    );
  } catch (error) {
    log.warn('signed-in workspace could not be made the active one', { error });
  }
}

/** No active workspace (the connect screen opens at launch); the slots stay. */
export function clearActiveInRegistry(): Promise<Registry> {
  return updateRegistry((r) => ({ ...r, active: null }));
}

/** Removes `id` from the registry (its storage must already be wiped). */
export function forgetWorkspace(id: string): Promise<Registry> {
  return updateRegistry((r) => ({
    v: 1,
    active: r.active === id ? null : r.active,
    ids: r.ids.filter((x) => x !== id),
  }));
}

export function newWorkspaceId(random: (count: number) => Uint8Array = getRandomBytes): string {
  const bytes = random(6);
  let hex = '';
  for (const byte of bytes) hex += byte.toString(16).padStart(2, '0');
  if (!WORKSPACE_ID_RE.test(hex) || hex === MIGRATED_WORKSPACE_ID) throw new Error('bad workspace id');
  return hex;
}

/** The slot's meta item, or null when absent or malformed. Rejects on a keychain error. */
export async function readWorkspaceMeta(id: string): Promise<WorkspaceMeta | null> {
  const raw = await getSecureItemIn(id, SECURE_KEYS.workspaceMeta);
  if (raw === null) return null;
  try {
    return MetaSchema.parse(JSON.parse(raw));
  } catch {
    return null;
  }
}

/**
 * The database file of a slot: from its meta, else the name a new slot would
 * get. Rejects when the meta cannot be READ: guessing then would open (and
 * fill) a second, empty file for the migrated slot, whose file is
 * `az-offline.db`, and a wipe would delete the wrong name.
 */
export async function workspaceDbName(id: string): Promise<string> {
  const meta = await readWorkspaceMeta(id);
  return meta?.dbName ?? dbNameFor(id);
}

/** When the slot's database was last open, or null when unknown. Rejects on a keychain error. */
export async function readWorkspaceUsedAt(id: string): Promise<number | null> {
  return (await readWorkspaceMeta(id))?.usedAt ?? null;
}

/**
 * Stamps the slot's meta with `at` as the last use of its database. A slot
 * without a readable meta is left alone (its meta is never guessed: it
 * names the database file). Rejects on a keychain error.
 */
export async function recordWorkspaceUse(id: string, at: number): Promise<void> {
  const meta = await readWorkspaceMeta(id);
  if (meta === null || !Number.isSafeInteger(at) || at < 0) return;
  await setSecureItemIn(id, SECURE_KEYS.workspaceMeta, JSON.stringify({ ...meta, usedAt: at }));
}

/** The id of the slot whose stored origin is `origin`, or null. Rejects when a slot's origin cannot be read. */
export async function findWorkspaceByOrigin(origin: string, registry?: Registry): Promise<string | null> {
  const r = registry ?? (await readRegistry());
  for (const id of r.ids) {
    if ((await getSecureItemIn(id, SECURE_KEYS.serverOrigin)) === origin) return id;
  }
  return null;
}

export interface CreateSlotOptions {
  now?: () => number;
  random?: (count: number) => Uint8Array;
}

/**
 * A new slot for `origin`: its origin and meta items first, then the registry
 * (the new id first; active only when no other workspace is, see
 * `activateSignedInWorkspace`). The limit and the one-slot-per-origin rule
 * are checked inside the registry's read-modify-write, so two overlapping
 * calls cannot both pass them (`TooManyWorkspacesError`,
 * `WorkspaceExistsError`; a stored origin that cannot be read rejects too).
 * On a failure nothing is registered and the items written are removed
 * again; the error is rethrown.
 */
export async function createWorkspaceSlot(origin: string, options: CreateSlotOptions = {}): Promise<string> {
  const registry = await readRegistry();
  if (registry.ids.length >= MAX_WORKSPACES) throw new TooManyWorkspacesError();
  let id = newWorkspaceId(options.random);
  for (let tries = 0; registry.ids.includes(id) && tries < 4; tries++) id = newWorkspaceId(options.random);
  if (registry.ids.includes(id)) throw new Error('could not allocate a workspace id');
  const meta: WorkspaceMeta = { v: 1, dbName: dbNameFor(id), addedAt: (options.now ?? Date.now)() };
  try {
    await setSecureItemIn(id, SECURE_KEYS.serverOrigin, origin);
    await setSecureItemIn(id, SECURE_KEYS.workspaceMeta, JSON.stringify(meta));
    await updateRegistry(async (r) => {
      if (r.ids.includes(id)) throw new Error('could not allocate a workspace id');
      if (r.ids.length >= MAX_WORKSPACES) throw new TooManyWorkspacesError();
      const existing = await findWorkspaceByOrigin(origin, r);
      if (existing !== null) throw new WorkspaceExistsError(existing);
      // Not active yet unless nothing else is: the previous workspace stays
      // the launch default until this one has signed in.
      return { v: 1, active: r.active ?? id, ids: [id, ...r.ids] };
    });
  } catch (error) {
    await deleteAllSecureItemsIn(id);
    throw error;
  }
  return id;
}

/** Deletes `az-offline.db` and its sidecars (the offline store registers it). */
export type LegacyDatabaseRemover = () => Promise<void>;
let legacyDatabaseRemover: LegacyDatabaseRemover | null = null;

export function registerLegacyDatabaseRemover(remover: LegacyDatabaseRemover | null): void {
  legacyDatabaseRemover = remover;
}

export type MigrationOutcome =
  /** Nothing in the old layout (or already migrated). */
  | 'none'
  /** The single workspace moved into slot `MIGRATED_WORKSPACE_ID`, signed in as before. */
  | 'migrated'
  /** The old layout held an unfinished wipe or no workspace: its items were deleted. */
  | 'discarded';

/**
 * Upgrade from the single-workspace layout (every item under its logical
 * name) to slot `MIGRATED_WORKSPACE_ID`, with no data loss and no new sign-in:
 *
 *   1. registry already present: the copy finished earlier; leftover old
 *      items (a crash after step 3) are deleted;
 *   2. an unfinished wipe (`az.wipe.pending`) or no stored origin: the old
 *      database file (`az-offline.db`, best effort) and the old items are
 *      deleted, nothing is migrated (the wipe completes);
 *   3. every old item is copied into the slot, the slot's meta names the old
 *      database file (`az-offline.db`, so the encrypted cache, outbox and sync
 *      cursors stay where they are), then the registry is written, then the
 *      old items are deleted.
 *
 * Idempotent: a crash anywhere re-runs it from the start next launch (the slot
 * id is fixed, so a second copy overwrites the first). A keychain read error
 * rejects (the caller retries when the device is unlocked).
 */
export async function migrateLegacyLayout(
  options: { now?: () => number; removeLegacyDatabase?: LegacyDatabaseRemover } = {},
): Promise<MigrationOutcome> {
  if ((await getDeviceItem(DEVICE_KEYS.workspaces)) !== null) {
    const leftovers = await anyLegacyItem();
    if (leftovers) await deleteAllLegacyItems();
    return 'none';
  }
  const origin = await getLegacyItem(SECURE_KEYS.serverOrigin);
  const pendingWipe = await getLegacyItem(SECURE_KEYS.wipePending);
  if (origin === null || pendingWipe !== null) {
    if (!(await anyLegacyItem())) return 'none';
    // The old wipe deleted this file through the offline store's wipe hook;
    // no slot will ever own it now. Before the items: their absence is what
    // says this branch already ran.
    const remove = options.removeLegacyDatabase ?? legacyDatabaseRemover;
    try {
      await remove?.();
    } catch (error) {
      log.warn('old offline database could not be deleted', { error });
    }
    const failures = await deleteAllLegacyItems();
    if (failures.length > 0) log.warn('old secure items could not all be deleted', { count: failures.length });
    return 'discarded';
  }
  const id = MIGRATED_WORKSPACE_ID;
  for (const key of REGISTERED_KEYS) {
    if (key === SECURE_KEYS.wipePending || key === SECURE_KEYS.workspaceMeta) continue;
    const value = await getLegacyItem(key);
    if (value !== null) await setSecureItemIn(id, key, value);
  }
  const meta: WorkspaceMeta = { v: 1, dbName: LEGACY_DB_NAME, addedAt: (options.now ?? Date.now)() };
  await setSecureItemIn(id, SECURE_KEYS.workspaceMeta, JSON.stringify(meta));
  await updateRegistry(() => ({ v: 1, active: id, ids: [id] }));
  const failures = await deleteAllLegacyItems();
  if (failures.length > 0) log.warn('old secure items not deleted yet; retried next launch', { count: failures.length });
  log.info('single-workspace storage migrated to a workspace slot');
  return 'migrated';
}

async function anyLegacyItem(): Promise<boolean> {
  for (const key of REGISTERED_KEYS) {
    if ((await getLegacyItem(key)) !== null) return true;
  }
  return false;
}

/** Test helper. */
export function __resetRegistryForTests(): void {
  snapshot = { ...EMPTY_REGISTRY, ids: [] };
  chain = Promise.resolve();
  listeners.clear();
}

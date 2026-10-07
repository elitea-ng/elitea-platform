import { abortAll } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { wipeCachesDirectory } from '@/security/cacheWipe';
import {
  SECURE_KEYS,
  type SecureKey,
  bindWorkspace,
  boundWorkspace,
  deleteAllSecureItemsIn,
  deleteSecureItemIn,
  getSecureItemIn,
  setSecureItemIn,
} from '@/security/secureStorage';
import { forgetWorkspace, readRegistry } from '@/security/workspaceRegistry';
import { getSession, resetSession, setSession, type EndReason } from '@/state/session';

/**
 * THE logout-and-wipe orchestrator: the one exit path from a session. It ends
 * the ACTIVE workspace only (the one bound to secure storage); every other
 * connected workspace keeps its slot, credential and data.
 *
 * Callers: Settings "Log out & wipe", a 401 `device_revoked`, a rejected PAT,
 * and (B3/B4) server switch, policy breach and "can't unlock → reset".
 *
 * Every step is attempted even when an earlier one fails; errors are collected
 * and returned, never thrown. Concurrent callers share one run, so ten parallel
 * 401s produce one wipe.
 *
 * Order (each step is attempted; a failure is recorded and the next one runs):
 *   1. crash-resume marker `az.wipe.pending` (in the workspace's namespace),
 *      then `session.ending`;
 *   2. abort every in-flight REST call and stream (abort registry);
 *   3. best-effort server revoke, capped at `REVOKE_TIMEOUT_MS` (B4 registers
 *      the native refresh-family revoke; a PAT belongs to the user and is never
 *      deleted server-side, so nothing is registered for it);
 *   4. wipe hooks: query cache, live turns, the encrypted store's connection;
 *      then the stored-wipe hooks for the workspace (its database file);
 *      then the app's caches directory, which holds the platform URL cache
 *      (`Library/Caches/<bundle id>/Cache.db` on iOS, DEF-R2);
 *   5. every registered secure-storage key of the workspace except the
 *      marker, then the workspace's entry in the registry;
 *   6. the in-memory session (no workspace bound any more);
 *   7. the marker, last, and only when every secure delete in step 5
 *      succeeded.
 *
 * A crash anywhere before step 7, or a secure delete that failed, leaves the
 * marker behind; launch hydration finds it and calls `resumePendingWipe()`,
 * which runs the stored part again (for that workspace only) before anything
 * is restored.
 */

export interface EndSessionOptions {
  reason: EndReason;
}

export interface EndSessionResult {
  reason: EndReason;
  /** One entry per failed step; empty when everything succeeded. */
  errors: { step: string; error: unknown }[];
  /**
   * False when a secure-storage item could not be deleted. The wipe-pending
   * marker is then kept, so the next launch retries the wipe; until then a
   * credential may still be stored and nothing may be connected over it.
   */
  storageWiped: boolean;
}

/**
 * Items a wipe keeps for its retry: the marker, and, when a stored-wipe hook
 * failed, what those hooks need to find their files again: the workspace meta
 * (it names the database file: the migrated slot's is `az-offline.db`, which
 * the retry could not guess) and the origin (the brand-font folder is named
 * after it). Neither is a secret; the marker keeps the slot from being opened.
 */
function keptForRetry(errors: readonly { step: string }[]): SecureKey[] {
  const fileLeft = errors.some((e) => e.step.startsWith('stored:'));
  return fileLeft
    ? [SECURE_KEYS.wipePending, SECURE_KEYS.workspaceMeta, SECURE_KEYS.serverOrigin]
    : [SECURE_KEYS.wipePending];
}

/** True when the step failure belongs to the secure-storage wipe (step 5). */
function isStorageFailure(step: string): boolean {
  return step === 'delete_secure_items' || step.startsWith('delete:') || step === 'forget_workspace' || step.startsWith('stored:');
}

/**
 * In-memory state of the active workspace registered by other modules (the
 * query cache, live turns, the native credential, policy, brand, the open
 * database connection, …). A hook must only forget what is in memory: it
 * runs when a session ends AND when the app switches to another workspace
 * (`suspendActiveWorkspace`), which must not destroy anything stored. Stored
 * data is deleted by `registerStoredWipeHook` hooks and the keychain wipe.
 */
export type WipeHook = () => void | Promise<void>;
const wipeHooks = new Map<string, WipeHook>();

/** Registers `hook` under `name` (re-registering replaces it). Returns an unregister function. */
export function registerWipeHook(name: string, hook: WipeHook): () => void {
  wipeHooks.set(name, hook);
  return () => {
    if (wipeHooks.get(name) === hook) wipeHooks.delete(name);
  };
}

/**
 * Deletes what a workspace keeps outside the keychain (its database file).
 * Runs for the active workspace in `endSession` and for any workspace in
 * `wipeStoredWorkspace` (a resumed wipe, removing a workspace that is not
 * active). Must not need the workspace to be bound.
 */
export type StoredWipeHook = (workspaceId: string) => void | Promise<void>;
const storedWipeHooks = new Map<string, StoredWipeHook>();

export function registerStoredWipeHook(name: string, hook: StoredWipeHook): () => void {
  storedWipeHooks.set(name, hook);
  return () => {
    if (storedWipeHooks.get(name) === hook) storedWipeHooks.delete(name);
  };
}

/** Upper bound for the best-effort server revoke; a wipe never waits longer for the network. */
export const REVOKE_TIMEOUT_MS = 3000;

/**
 * Server-side revoke of the current credential. It runs while the credential
 * is still available, after in-flight work is aborted and before anything is
 * deleted. The signal aborts when the cap is reached.
 */
export type RevokeHook = (signal: AbortSignal) => Promise<void>;
let revokeHook: RevokeHook | null = null;

/** Sets (or with null clears) the server revoke. Returns an unregister function. */
export function registerRevokeHook(hook: RevokeHook | null): () => void {
  revokeHook = hook;
  return () => {
    if (revokeHook === hook) revokeHook = null;
  };
}

async function revokeWithCap(hook: RevokeHook): Promise<void> {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | null = null;
  const cap = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      controller.abort(new Error('revoke_timeout'));
      reject(new Error('revoke_timeout'));
    }, REVOKE_TIMEOUT_MS);
  });
  try {
    await Promise.race([hook(controller.signal), cap]);
  } finally {
    if (timer !== null) clearTimeout(timer);
  }
}

let inFlight: Promise<EndSessionResult> | null = null;

export function endSession(options: EndSessionOptions): Promise<EndSessionResult> {
  if (inFlight) return inFlight;
  inFlight = run(options).finally(() => {
    inFlight = null;
  });
  return inFlight;
}

export function isEndingSession(): boolean {
  return inFlight !== null;
}

let suspending = false;

/** True while the active workspace is being put aside for another one. */
export function isSuspendingWorkspace(): boolean {
  return suspending;
}

/**
 * Puts the active workspace aside WITHOUT wiping anything: in-flight work is
 * aborted, the in-memory state of every module is forgotten (the same hooks
 * as a wipe), no workspace is bound any more and the session is signed out
 * in memory. Its credential, database, outbox, sync cursors, brand and policy
 * stay stored in its slot, ready for the next switch back. Waits for an
 * `endSession` already under way. Never throws.
 */
export async function suspendActiveWorkspace(): Promise<void> {
  if (inFlight) await inFlight.catch(() => undefined);
  suspending = true;
  try {
    // Unbind first: from here on nothing can be written to the old slot, and
    // a write already under way keeps the namespace it started with.
    bindWorkspace(null);
    try {
      abortAll('workspace_switched');
    } catch (error) {
      log.warn('abort on switch failed', { error });
    }
    for (const [name, hook] of [...wipeHooks]) {
      try {
        await hook();
      } catch (error) {
        log.warn('reset hook failed on switch', { name, error });
      }
    }
    const { switching, returnWorkspaceId } = getSession();
    resetSession({ endReason: null, lastOriginHint: null, switching, returnWorkspaceId });
  } finally {
    suspending = false;
  }
}

/**
 * The server revoked a workspace that is no longer the active one (an answer
 * that landed while it was being put aside, or for a workspace put aside
 * already). Nothing of it is in memory any more and the session in use is
 * another one, so `endSession` would end the wrong workspace: only its
 * crash-resume marker is written. Opening it again (`switchToWorkspace`) or
 * the next launch (`resumePendingWipes`) then wipes it instead of restoring
 * it, and a token rotation still under way for it is dropped. Never throws.
 */
export async function markWorkspaceRevoked(workspaceId: string): Promise<void> {
  try {
    if ((await getSecureItemIn(workspaceId, SECURE_KEYS.wipePending)) !== null) return;
    await setSecureItemIn(
      workspaceId,
      SECURE_KEYS.wipePending,
      JSON.stringify({ reason: 'device_revoked', startedAt: Date.now() }),
    );
  } catch (error) {
    log.warn('revoked workspace could not be marked for its wipe', { error });
  }
}

async function run({ reason }: EndSessionOptions): Promise<EndSessionResult> {
  const errors: EndSessionResult['errors'] = [];
  const step = async (name: string, fn: () => unknown) => {
    try {
      await fn();
    } catch (error) {
      errors.push({ step: name, error });
    }
  };

  const originBefore = getSession().serverOrigin;
  // The workspace being ended. Null only when nothing is bound (signed out):
  // then only the in-memory state is reset.
  const workspaceId = boundWorkspace();

  // 1. Crash-resume marker first, then refuse new work and show "Signing out…".
  if (workspaceId !== null) {
    await step('mark_pending', () =>
      setSecureItemIn(workspaceId, SECURE_KEYS.wipePending, JSON.stringify({ reason, startedAt: Date.now() })),
    );
  }
  await step('mark_ending', () => setSession({ ending: true, endReason: reason }));

  // 2. Stop everything in flight.
  await step('abort_in_flight', () => abortAll(`session_ended:${reason}`));

  // 3. Server-side revoke, best effort and capped. Never for a revocation the
  //    server itself announced (device_revoked): the credential is already dead.
  const revoke = revokeHook;
  if (revoke && reason !== 'device_revoked') {
    await step('server_revoke', () => revokeWithCap(revoke));
  }

  // 4. Caches and other registered local state, then the workspace's files.
  for (const [name, hook] of [...wipeHooks]) {
    await step(`hook:${name}`, hook);
  }
  if (workspaceId !== null) {
    for (const [name, hook] of [...storedWipeHooks]) {
      await step(`stored:${name}`, () => hook(workspaceId));
    }
  }
  // Plaintext HTTP responses (tokens, transcripts) a URL cache may have kept.
  await step('clear_caches', () => wipeCachesDirectory());

  // 5. Every registered secure-storage key of the workspace except the marker
  //    (and its meta while a file is left: it names that file for the retry),
  //    then its registry entry (only once nothing of it is left).
  if (workspaceId !== null) {
    await step('delete_secure_items', async () => {
      const failures = await deleteAllSecureItemsIn(workspaceId, keptForRetry(errors));
      for (const failure of failures) {
        errors.push({ step: `delete:${failure.key}`, error: failure.error });
      }
    });
    if (!errors.some((e) => isStorageFailure(e.step))) {
      await step('forget_workspace', () => forgetWorkspace(workspaceId));
    }
  }

  // 6. In-memory session. A rejected credential (not a revocation) keeps a
  //    non-secret origin hint so signing in again is quick.
  await step('reset_session', () => {
    bindWorkspace(null);
    resetSession({
      endReason: reason,
      lastOriginHint: reason === 'credential_rejected' ? originBefore : null,
      endedOrigin: originBefore,
    });
  });

  // 7. Marker last, and only once every secure item is gone: a crash before
  //    this point, or a key that could not be deleted, re-runs the wipe at
  //    launch before any stored credential is read.
  const storageWiped = !errors.some((e) => isStorageFailure(e.step));
  if (storageWiped && workspaceId !== null) {
    await step('clear_pending', () => deleteSecureItemIn(workspaceId, SECURE_KEYS.wipePending));
  }

  if (errors.length > 0) {
    log.warn('endSession finished with errors', {
      reason,
      steps: errors.map((e) => e.step),
    });
  }
  return { reason, errors, storageWiped };
}

const END_REASONS: readonly EndReason[] = [
  'user_logout',
  'device_revoked',
  'credential_rejected',
  'server_switch',
  'policy',
  'reset',
];

function parseReason(raw: string): EndReason {
  try {
    const parsed: unknown = JSON.parse(raw);
    const candidate = (parsed as { reason?: unknown } | null)?.reason;
    if (typeof candidate === 'string' && (END_REASONS as readonly string[]).includes(candidate)) {
      return candidate as EndReason;
    }
  } catch {
    // unreadable marker: still a wipe under way
  }
  return 'reset';
}

export interface StoredWipeResult {
  /** Every stored item of the workspace is gone and it left the registry. */
  wiped: boolean;
  errors: { step: string; error: unknown }[];
}

/**
 * Wipes a workspace that is NOT active (nothing of it is in memory): the
 * removal of a workspace from the switcher, and a wipe resumed at launch.
 * Marker first; an optional server revoke (capped, best effort); the stored
 * wipe hooks (its database file); every secure item; the registry entry; the
 * marker last, only when everything else succeeded. Never throws.
 */
export async function wipeStoredWorkspace(
  workspaceId: string,
  options: { reason: EndReason; revoke?: (signal: AbortSignal) => Promise<void> },
): Promise<StoredWipeResult> {
  const errors: StoredWipeResult['errors'] = [];
  const step = async (name: string, fn: () => unknown) => {
    try {
      await fn();
    } catch (error) {
      errors.push({ step: name, error });
    }
  };
  await step('mark_pending', () =>
    setSecureItemIn(workspaceId, SECURE_KEYS.wipePending, JSON.stringify({ reason: options.reason, startedAt: Date.now() })),
  );
  if (options.revoke) {
    const revoke = options.revoke;
    await step('server_revoke', () => revokeWithCap(revoke));
  }
  for (const [name, hook] of [...storedWipeHooks]) {
    await step(`stored:${name}`, () => hook(workspaceId));
  }
  await step('delete_secure_items', async () => {
    const failures = await deleteAllSecureItemsIn(workspaceId, keptForRetry(errors));
    for (const failure of failures) errors.push({ step: `delete:${failure.key}`, error: failure.error });
  });
  const stored = () => !errors.some((e) => isStorageFailure(e.step));
  if (stored()) await step('forget_workspace', () => forgetWorkspace(workspaceId));
  const wiped = stored();
  if (wiped) await step('clear_pending', () => deleteSecureItemIn(workspaceId, SECURE_KEYS.wipePending));
  if (errors.length > 0) log.warn('workspace wipe finished with errors', { steps: errors.map((e) => e.step) });
  return { wiped, errors };
}

export interface ResumedWipe {
  workspaceId: string;
  reason: EndReason;
  /** False: the wipe is still incomplete (its marker stays); nothing of the workspace may be restored. */
  wiped: boolean;
}

/**
 * Launch-time half of the crash-resume marker: every workspace in the
 * registry whose `az.wipe.pending` is still stored is wiped again before
 * anything is read or shown. Only those workspaces are touched.
 *
 * A marker that cannot be parsed still triggers the wipe (reason `reset`):
 * its presence alone means a wipe was under way. A marker that cannot be READ
 * (keychain unavailable) rejects, so the caller retries later instead of
 * guessing.
 */
export async function resumePendingWipes(): Promise<ResumedWipe[]> {
  const registry = await readRegistry();
  const resumed: ResumedWipe[] = [];
  for (const workspaceId of registry.ids) {
    const raw = await getSecureItemIn(workspaceId, SECURE_KEYS.wipePending);
    if (raw === null) continue;
    const reason = parseReason(raw);
    log.info('resuming an interrupted wipe', { reason });
    const { wiped } = await wipeStoredWorkspace(workspaceId, { reason });
    resumed.push({ workspaceId, reason, wiped });
  }
  return resumed;
}

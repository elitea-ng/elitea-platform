import type { ReactNode } from 'react';

import { revokeStoredSession } from '@/auth/nativeSession';
import { log } from '@/lib/log';
import { parseConnectLink, type ParseConnectLinkOptions } from '@/linking/parseConnectLink';
import { endSession, suspendActiveWorkspace, wipeStoredWorkspace } from '@/security/endSession';
import { SECURE_KEYS, bindWorkspace, boundWorkspace, getSecureItemIn } from '@/security/secureStorage';
import {
  activateInRegistry,
  WorkspaceExistsError,
  createWorkspaceSlot,
  findWorkspaceByOrigin,
  readRegistry,
} from '@/security/workspaceRegistry';
import { restoreWorkspace } from '@/state/hydrate';
import { getSession, setSession, useSession, type EndReason } from '@/state/session';
import { refreshWorkspaceList } from '@/state/workspaceList';

/**
 * Several connected workspaces, one active at a time.
 *
 * Every workspace keeps its own slot (secure-store namespace, encrypted
 * database and key, brand, policy, outbox, sync cursors, device session).
 * Switching never wipes: the active workspace is put aside
 * (`suspendActiveWorkspace`: in-flight work aborted, in-memory state
 * forgotten, nothing stored touched) and the other one is restored from its
 * slot exactly as at launch. "Log out & wipe", a revoke or a rejected
 * credential end ONE workspace (`endSession`); the others stay.
 *
 * Transitions (switch, add, remove) are serialised.
 */

/**
 * Told before the active workspace is put aside for another one, and after
 * the transition (with whether the new session is signed in). The device
 * lock uses it to carry the user's presence across a switch.
 */
export type SwitchEvent = { phase: 'before' } | { phase: 'after'; signedIn: boolean };
type SwitchListener = (event: SwitchEvent) => void;
const switchListeners = new Set<SwitchListener>();

export function onWorkspaceSwitch(listener: SwitchListener): () => void {
  switchListeners.add(listener);
  return () => {
    switchListeners.delete(listener);
  };
}

let chain: Promise<unknown> = Promise.resolve();
/** Transitions under way that put the active session aside or replace it. */
let replacing = 0;

/**
 * Runs `work` after every earlier transition. `replacesActive`: the work puts
 * the active session aside or replaces it (switch, add, connect); removing a
 * workspace that is not active does not, and must not look like it does.
 */
function serial<T>(work: () => Promise<T>, { replacesActive }: { replacesActive: boolean }): Promise<T> {
  const next = chain.then(async () => {
    if (replacesActive) replacing++;
    try {
      return await work();
    } finally {
      if (replacesActive) replacing--;
    }
  });
  chain = next.catch(() => undefined);
  return next;
}

/**
 * True while the ACTIVE session is being put aside or replaced (a switch, an
 * add or a connect). The device lock ignores policy changes meanwhile (the
 * policy passes through the strict defaults), so this is false while a
 * workspace that is not active is removed: the active one stays in use and a
 * tightening of its policy must apply at once.
 */
export function isSwitchingWorkspace(): boolean {
  return replacing > 0 || getSession().switching;
}

/** Kept for source compatibility; the session store needs no provider. */
export function WorkspaceProvider({ children }: { children: ReactNode }) {
  return children;
}

function tell(event: SwitchEvent): void {
  for (const listener of [...switchListeners]) {
    try {
      listener(event);
    } catch (error) {
      log.warn('workspace switch listener failed', { error });
    }
  }
}

/** Puts the active workspace aside (nothing wiped). */
async function suspend(): Promise<void> {
  if (boundWorkspace() === null && getSession().serverOrigin === null) return;
  await suspendActiveWorkspace();
}

/**
 * Restores slot `id` into the session. A slot with an unfinished wipe is
 * wiped (not restored). Returns false when it could not be opened.
 */
async function activate(id: string): Promise<boolean> {
  const marker = await getSecureItemIn(id, SECURE_KEYS.wipePending);
  if (marker !== null) {
    await wipeStoredWorkspace(id, { reason: 'reset' });
    return false;
  }
  const restored = await restoreWorkspace(id);
  if (!restored) return false;
  await activateInRegistry(id);
  return true;
}

/**
 * Makes workspace `id` the active one. The current one is put aside, never
 * wiped. Resolves false when `id` could not be opened (then no workspace is
 * active and the connect screen lists the others).
 */
export function switchToWorkspace(id: string): Promise<boolean> {
  return serial(() => switchNow(id), { replacesActive: true });
}

/** `switchToWorkspace` inside a transition already under way. */
async function switchNow(id: string): Promise<boolean> {
  if (getSession().workspaceId === id && boundWorkspace() === id) return true;
  const registry = await readRegistry().catch(() => null);
  if (registry !== null && !registry.ids.includes(id)) return false;
  setSession({ switching: true });
  tell({ phase: 'before' });
  try {
    await suspend();
    try {
      const ok = await activate(id);
      if (ok) setSession({ returnWorkspaceId: null, endReason: null, endedOrigin: null });
      return ok;
    } catch (error) {
      // Secure storage unreadable (device locked mid-switch): nothing active.
      log.warn('workspace could not be opened', { error });
      bindWorkspace(null);
      setSession({ serverOrigin: null, workspaceId: null, credential: null, projectId: null });
      return false;
    }
  } finally {
    tell({ phase: 'after', signedIn: getSession().credential !== null });
    setSession({ switching: false });
    void refreshWorkspaceList();
  }
}

/**
 * "Add workspace": the active workspace is put aside (kept, still the one a
 * relaunch opens) and the connect screen opens, checking `origin` straight
 * away when one is given (a connect link). Its "Back" returns to the
 * workspace that was active.
 */
export function beginAddWorkspace(origin: string | null = null): Promise<void> {
  return serial(async () => {
    const previous = getSession().workspaceId;
    setSession({ switching: true, returnWorkspaceId: previous });
    tell({ phase: 'before' });
    try {
      await suspend();
    } finally {
      tell({ phase: 'after', signedIn: false });
      setSession({ switching: false, pendingConnectOrigin: origin, returnWorkspaceId: previous });
    }
  }, { replacesActive: true });
}

/**
 * Connects `origin` (the connect screen's confirmation). A workspace already
 * stored for the same origin is switched to, not duplicated; otherwise a new
 * slot is created, bound and made active. Resolves false when nothing could
 * be stored, or when the stored origins cannot be read (that is not "not
 * connected": a second slot for the same host would follow).
 */
export function connectWorkspace(origin: string): Promise<boolean> {
  return serial(async () => {
    let existing: string | null;
    try {
      existing = await findWorkspaceByOrigin(origin);
    } catch (error) {
      log.warn('connected workspaces could not be read', { error });
      return false;
    }
    if (existing !== null) return switchNow(existing);
    if (boundWorkspace() !== null) await suspend();
    let id: string;
    try {
      id = await createWorkspaceSlot(origin);
    } catch (error) {
      if (error instanceof WorkspaceExistsError) return switchNow(error.id);
      log.warn('could not store the workspace origin', { error });
      return false;
    }
    bindWorkspace(id);
    setSession({
      serverOrigin: origin,
      workspaceId: id,
      endReason: null,
      endedOrigin: null,
      lastOriginHint: null,
      pendingConnectOrigin: null,
      returnWorkspaceId: null,
    });
    void refreshWorkspaceList();
    return true;
  }, { replacesActive: true });
}

/**
 * Removes workspace `id` from this device: the active one is logged out and
 * wiped (`endSession`), any other one is revoked (best effort) and wiped from
 * its slot without touching the active session. Resolves true when nothing of
 * it is left. Not a replacing transition: removing the active workspace is an
 * `endSession` (`isEndingSession` covers it), removing another one leaves the
 * active session in use.
 */
export function removeWorkspace(id: string, reason: EndReason = 'user_logout'): Promise<boolean> {
  return serial(async () => {
    if (boundWorkspace() === id) {
      const { storageWiped } = await endSession({ reason });
      void refreshWorkspaceList();
      return storageWiped;
    }
    const { wiped } = await wipeStoredWorkspace(id, {
      reason,
      revoke: (signal) => revokeStoredSession(id, signal),
    });
    void refreshWorkspaceList();
    return wiped;
  }, { replacesActive: false });
}

/** "Can't unlock → reset": every workspace on this device is logged out and wiped. */
export async function removeAllWorkspaces(): Promise<boolean> {
  let ok = (await endSession({ reason: 'reset' })).storageWiped;
  const registry = await readRegistry().catch(() => null);
  for (const id of registry?.ids ?? []) {
    ok = (await wipeStoredWorkspace(id, { reason: 'reset' })).wiped && ok;
  }
  void refreshWorkspaceList();
  return ok;
}

/**
 * "Back" on the connect screen after "Add workspace": the workspace that was
 * active before. Resolves false when there is none (or it is gone).
 */
export async function returnToPreviousWorkspace(): Promise<boolean> {
  const previous = getSession().returnWorkspaceId;
  if (previous === null) return false;
  const ok = await switchToWorkspace(previous);
  if (!ok) setSession({ returnWorkspaceId: null });
  return ok;
}

/**
 * Leaving the sign-in screen for another workspace. A slot that never
 * signed in holds nothing worth keeping: it is removed. One that did (its
 * credential was refused, say) stays listed, signed out.
 */
export async function leaveSignIn(): Promise<void> {
  const id = boundWorkspace();
  if (id === null) return;
  const everSignedIn =
    (await getSecureItemIn(id, SECURE_KEYS.dbKey).catch(() => null)) !== null ||
    (await getSecureItemIn(id, SECURE_KEYS.selfUser).catch(() => null)) !== null;
  if (everSignedIn) await beginAddWorkspace();
  else await removeWorkspace(id, 'server_switch');
}

const clear = async () => {
  const id = boundWorkspace();
  if (id !== null) await removeWorkspace(id);
  else await endSession({ reason: 'user_logout' });
};

interface WorkspaceState {
  serverOrigin: string | null;
  workspaceId: string | null;
  /** Connects a workspace (see `connectWorkspace`). */
  connect: (origin: string) => Promise<boolean>;
  /** Logs out of the ACTIVE workspace and wipes only its data. */
  clear: () => Promise<void>;
}

export function useWorkspace(): WorkspaceState {
  const serverOrigin = useSession((s) => s.serverOrigin);
  const workspaceId = useSession((s) => s.workspaceId);
  return { serverOrigin, workspaceId, connect: connectWorkspace, clear };
}

/** What to do with a URL the OS opened the app with. */
export type IncomingLinkAction =
  /** Not a connect link (OAuth redirect, dev-client URL, …), or unusable. */
  | { kind: 'ignore' }
  /** No workspace active: the connect screen handles it (it reads the `server` param). */
  | { kind: 'connect'; origin: string }
  /** The workspace already in use: nothing to do. */
  | { kind: 'same' }
  /** A workspace already connected on this device: ask, then `switchToWorkspace(workspaceId)`. */
  | { kind: 'switch'; from: string; origin: string; workspaceId: string }
  /** A new workspace while one is active: ask, then `beginAddWorkspace(origin)`. Nothing is wiped. */
  | { kind: 'add'; from: string; origin: string };

export function classifyIncomingLink(
  url: string | null,
  currentOrigin: string | null,
  options: ParseConnectLinkOptions,
  known: readonly { id: string; origin: string }[] = [],
): IncomingLinkAction {
  if (!url) return { kind: 'ignore' };
  const isConnectLink = options.connectSchemes.some((scheme) =>
    url.toLowerCase().startsWith(`${scheme.toLowerCase()}://connect`),
  );
  if (!isConnectLink) return { kind: 'ignore' };
  const parsed = parseConnectLink(url, options);
  if (!parsed.ok) return { kind: 'ignore' };
  if (currentOrigin === null) return { kind: 'connect', origin: parsed.origin };
  if (currentOrigin === parsed.origin) return { kind: 'same' };
  const existing = known.find((w) => w.origin === parsed.origin);
  if (existing) return { kind: 'switch', from: currentOrigin, origin: parsed.origin, workspaceId: existing.id };
  return { kind: 'add', from: currentOrigin, origin: parsed.origin };
}

/** Test helper. */
export function __resetWorkspaceTransitionsForTests(): void {
  chain = Promise.resolve();
  replacing = 0;
  switchListeners.clear();
}

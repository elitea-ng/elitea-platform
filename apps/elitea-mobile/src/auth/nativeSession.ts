import { z } from 'zod';

import { getClientVersion } from '@/api/clientVersion';
import { isSameOrigin } from '@/api/paths';
import { setCredentialRefresher, type CredentialRefresher, type RefreshVerdict } from '@/auth/credentialRefresh';
import { refreshTokens, revokeToken, type TokenOutcome, type TokenResponse } from '@/auth/tokenEndpoint';
import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import {
  endSession,
  isSuspendingWorkspace,
  registerRevokeHook,
  registerStoredWipeHook,
  registerWipeHook,
} from '@/security/endSession';
import {
  NoWorkspaceError,
  SECURE_KEYS,
  boundWorkspace,
  getSecureItem,
  getSecureItemIn,
  setSecureItemIn,
} from '@/security/secureStorage';
import { activateSignedInWorkspace } from '@/security/workspaceRegistry';
import { getBearerToken, getSession, setCredential, setSession } from '@/state/session';

/**
 * The native sign-in credential (ADR-0025 WP2/WP3): an opaque access token
 * (minutes) and a rotating refresh token (the device session).
 *
 * Storage (secure store, device-only):
 *   az.auth.refresh  {origin, endpoints, refresh token, its expiry, device id}
 *   az.auth.access   {access token, its expiry}
 *   az.auth.kind     'oauth', written last at sign-in (the commit marker)
 *
 * Refresh rules:
 *   - Single-flight: concurrent callers (a 401 storm, a stream and a list
 *     reconnecting together) share one refresh.
 *   - Persist BEFORE use: the rotated pair is written (refresh token first)
 *     before anything is sent with it, so a crash cannot leave the app holding
 *     a token the server already replaced. If the write fails the pair is
 *     still used from memory (discarding it would make the next refresh a
 *     reuse, which revokes the device) and the write is retried later.
 *   - Re-delivery grace: when the token endpoint gives no answer the rotation
 *     may still have happened. The SAME refresh token is presented again,
 *     inside the server's re-delivery window (30 s by default), which answers
 *     with the same successor pair instead of treating it as reuse.
 *   - A token set belongs to the workspace it was issued for, and is only ever
 *     stored into that workspace's slot. Switching to another workspace is not
 *     a wipe: a rotation that finishes after the switch (or one that so far
 *     lived only in memory) is still stored into its own slot, never
 *     activated; otherwise that slot would keep a refresh token the server
 *     already consumed, and the next use of it would be a reuse (revoke and
 *     wipe). A slot whose wipe has begun gets nothing.
 *   - `device_revoked` (revoked, expired, deactivated or reused) wipes via
 *     `endSession('device_revoked')`; an invalid grant or an unknown client
 *     ends the session as `credential_rejected`. A network failure changes
 *     nothing.
 */

/** Refresh this long before the access token expires. */
export const ACCESS_REFRESH_SKEW_MS = 60_000;
/** Total time spent re-presenting one refresh token after lost answers (inside the server's 30 s window). */
export const REDELIVERY_BUDGET_MS = 20_000;
export const REDELIVERY_MAX_ATTEMPTS = 3;
/** Longest a wipe waits for a token write already under way. */
export const WIPE_WRITE_WAIT_MS = 5_000;
/**
 * Longest opening a workspace again waits for a refresh of it still under
 * way from before it was put aside (the re-delivery budget plus a request).
 */
export const DETACHED_REFRESH_WAIT_MS = 30_000;
const REDELIVERY_BASE_DELAY_MS = 1_000;

export interface NativeEndpoints {
  tokenEndpoint: string;
  revocationEndpoint: string;
}

export interface NativeTokenSet extends NativeEndpoints {
  /** The workspace slot this credential belongs to (and is stored in). */
  workspaceId: string;
  origin: string;
  clientId: string;
  accessToken: string;
  accessExpiresAt: number;
  refreshToken: string;
  refreshExpiresAt: number;
  deviceId: string;
}

const RefreshRecord = z.object({
  v: z.literal(1),
  origin: z.string(),
  clientId: z.string(),
  tokenEndpoint: z.string(),
  revocationEndpoint: z.string(),
  refreshToken: z.string().min(1),
  refreshExpiresAt: z.number(),
  deviceId: z.string(),
});
const AccessRecord = z.object({ v: z.literal(1), accessToken: z.string().min(1), accessExpiresAt: z.number() });

export interface NativeSessionDeps {
  now: () => number;
  sleep: (ms: number) => Promise<void>;
  refresh: typeof refreshTokens;
  revoke: typeof revokeToken;
  clientVersion: () => string;
}

const defaultDeps: NativeSessionDeps = {
  now: Date.now,
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  refresh: refreshTokens,
  revoke: revokeToken,
  clientVersion: getClientVersion,
};

let deps: NativeSessionDeps = defaultDeps;
let current: NativeTokenSet | null = null;
let unpersisted = false;
let refreshing: Promise<RefreshVerdict> | null = null;
/** The set the in-flight refresh started from. */
let refreshingSet: NativeTokenSet | null = null;
/**
 * Sets put aside by a workspace switch (not a wipe) while a refresh of theirs
 * is still under way: its result is stored into their slot, never activated.
 */
const detached = new Set<NativeTokenSet>();
/**
 * A rotated pair of a workspace that is not active and could not be stored
 * (keychain error): the next restore of that workspace uses it (and stores
 * it) instead of the superseded refresh token in the keychain.
 */
const parked = new Map<string, NativeTokenSet>();
/** Refreshes of workspaces put aside, by workspace id: their restore waits for them. */
const detachedRefreshes = new Map<string, Promise<unknown>>();
let unregisterRevoke: (() => void) | null = null;

/** Told about every token response (sign-in and refresh): it carries the client policy. */
type GrantListener = (response: TokenResponse) => void;
const grantListeners = new Set<GrantListener>();

export function onTokenGrant(listener: GrantListener): () => void {
  grantListeners.add(listener);
  return () => {
    grantListeners.delete(listener);
  };
}

function notifyGrant(response: TokenResponse): void {
  for (const listener of [...grantListeners]) {
    try {
      listener(response);
    } catch (error) {
      log.warn('token grant listener failed', { error });
    }
  }
}

export function tokenSetFrom(
  response: TokenResponse,
  receivedAt: number,
  base: Pick<NativeTokenSet, 'workspaceId' | 'origin' | 'clientId' | 'tokenEndpoint' | 'revocationEndpoint'>,
): NativeTokenSet {
  return {
    ...base,
    accessToken: response.access_token,
    accessExpiresAt: receivedAt + response.expires_in * 1000,
    refreshToken: response.refresh_token,
    refreshExpiresAt: receivedAt + response.refresh_token_expires_in * 1000,
    deviceId: response.device_id,
  };
}

/**
 * Token writes in progress. The wipe hook waits for them, so endSession's
 * secure-storage delete (which runs after every hook) always comes AFTER a
 * write that started before the wipe; otherwise a rotated refresh token
 * could land in the keychain once the wipe had already cleared it.
 */
const pendingWrites = new Set<Promise<unknown>>();

function tracked<T>(write: Promise<T>): Promise<T> {
  const settled = write.then(
    () => undefined,
    () => undefined,
  );
  pendingWrites.add(settled);
  void settled.then(() => pendingWrites.delete(settled));
  return write;
}

/** Writes the refresh record first, then the access record. Throws on a storage failure. */
function persistTokens(set: NativeTokenSet): Promise<void> {
  return tracked(writeTokens(set));
}

async function writeTokens(set: NativeTokenSet): Promise<void> {
  await setSecureItemIn(
    set.workspaceId,
    SECURE_KEYS.refreshToken,
    JSON.stringify({
      v: 1,
      origin: set.origin,
      clientId: set.clientId,
      tokenEndpoint: set.tokenEndpoint,
      revocationEndpoint: set.revocationEndpoint,
      refreshToken: set.refreshToken,
      refreshExpiresAt: set.refreshExpiresAt,
      deviceId: set.deviceId,
    }),
  );
  await setSecureItemIn(
    set.workspaceId,
    SECURE_KEYS.accessToken,
    JSON.stringify({ v: 1, accessToken: set.accessToken, accessExpiresAt: set.accessExpiresAt }),
  );
}

/**
 * Stores the set of a workspace that was put aside, unless that workspace's
 * wipe has begun (its marker is written before anything is deleted, and the
 * stored-wipe hook waits for writes already under way). False: dropped.
 */
function persistDetached(set: NativeTokenSet, owner: NativeTokenSet): Promise<boolean> {
  return tracked(
    (async () => {
      if ((await getSecureItemIn(set.workspaceId, SECURE_KEYS.wipePending)) !== null) {
        detached.delete(owner);
        return false;
      }
      await writeTokens(set);
      return true;
    })(),
  );
}

/** The owner is still worth storing for: the active set, or one put aside by a switch. */
function alive(owner: NativeTokenSet): boolean {
  return current === owner || detached.has(owner);
}

/**
 * Stores `set`, retrying once. `owner` is the session the write belongs to:
 * once it is neither current nor put aside by a switch (a wipe cleared it, or
 * another sign-in replaced it) no further attempt starts, so a retry can never
 * land after the wipe hook stopped waiting and endSession deleted the keys.
 */
async function persistWithRetry(set: NativeTokenSet, owner: NativeTokenSet): Promise<boolean> {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (!alive(owner)) return false;
    try {
      if (current === owner) {
        await persistTokens(set);
        return true;
      }
      return await persistDetached(set, owner);
    } catch (error) {
      log.warn('rotated tokens could not be stored', { error, attempt });
    }
  }
  return false;
}

/**
 * The newest pair of a workspace that was put aside: into its slot, or parked
 * in memory for its next restore when the keychain refuses it.
 */
async function storeDetached(set: NativeTokenSet, owner: NativeTokenSet): Promise<void> {
  if (await persistWithRetry(set, owner)) {
    if (parked.get(set.workspaceId) === owner) parked.delete(set.workspaceId);
    return;
  }
  if (detached.has(owner)) {
    log.warn('rotated tokens of a workspace put aside kept in memory until it is opened again');
    parked.set(set.workspaceId, set);
  }
}

function activate(set: NativeTokenSet): void {
  current = set;
  setCredential({ kind: 'oauth', token: set.accessToken });
  unregisterRevoke?.();
  unregisterRevoke = registerRevokeHook(async (signal) => {
    const active = current;
    if (active === null) return;
    await deps.revoke(
      active.origin,
      active.revocationEndpoint,
      { token: active.refreshToken, clientId: active.clientId },
      { signal },
    );
  });
  setCredentialRefresher(refresher);
}

/** The workspace a sign-in was for is no longer the active one; its tokens are stored in its slot. */
export class WorkspaceChangedError extends Error {
  constructor() {
    super('workspace changed during sign-in');
    this.name = 'WorkspaceChangedError';
  }
}

/**
 * Sign-in finished with a token response: store it (refresh record, access
 * record, then the credential kind as the commit marker) and make it the
 * session credential. Throws when it cannot be stored; nothing is activated
 * then.
 */
export async function startNativeSession(
  response: TokenResponse,
  receivedAt: number,
  base: Pick<NativeTokenSet, 'origin' | 'clientId' | 'tokenEndpoint' | 'revocationEndpoint'>,
): Promise<void> {
  const workspaceId = boundWorkspace();
  if (workspaceId === null) throw new NoWorkspaceError();
  const sessionWorkspace = getSession().workspaceId;
  const set = tokenSetFrom(response, receivedAt, { ...base, workspaceId });
  await persistTokens(set);
  await tracked(setSecureItemIn(workspaceId, SECURE_KEYS.credentialKind, 'oauth'));
  // A switch can land while the tokens are being stored (sign-in completion is
  // not one of the serialised transitions). They stay in their own slot, which
  // then opens signed in; activating them now would make this workspace's
  // token the credential of the one switched to, sent to its server.
  if (boundWorkspace() !== workspaceId || getSession().workspaceId !== sessionWorkspace) {
    throw new WorkspaceChangedError();
  }
  parked.delete(workspaceId);
  unpersisted = false;
  activate(set);
  notifyGrant(response);
  setSession({ returnWorkspaceId: null });
  await activateSignedInWorkspace(workspaceId);
}

/**
 * Launch: restores the stored native credential for `origin`. Returns false
 * when there is none usable (the caller then signs in again). The endpoints
 * must still be on the workspace origin. A missing or unreadable access
 * record only means the first request refreshes.
 */
export async function restoreNativeSession(origin: string): Promise<boolean> {
  const workspaceId = boundWorkspace();
  if (workspaceId === null) throw new NoWorkspaceError();
  // A refresh from before this workspace was put aside: its answer replaces
  // the stored refresh token, so read the slot only once it has landed.
  const inflight = detachedRefreshes.get(workspaceId);
  if (inflight !== undefined) {
    await waitAtMost(inflight, DETACHED_REFRESH_WAIT_MS);
    if (boundWorkspace() !== workspaceId) throw new NoWorkspaceError();
  }
  const newer = parked.get(workspaceId);
  if (newer !== undefined) {
    parked.delete(workspaceId);
    if (newer.origin === origin) {
      // Newer than the keychain (its write failed while the workspace was put
      // aside): use it, and store it on the next request.
      activate(newer);
      unpersisted = true;
      return true;
    }
  }
  const raw = await getSecureItem(SECURE_KEYS.refreshToken);
  if (raw === null) return false;
  let record: z.infer<typeof RefreshRecord>;
  try {
    record = RefreshRecord.parse(JSON.parse(raw));
  } catch {
    log.warn('stored native credential unreadable');
    return false;
  }
  if (
    record.origin !== origin ||
    !isSameOrigin(record.tokenEndpoint, origin) ||
    !isSameOrigin(record.revocationEndpoint, origin)
  ) {
    log.warn('stored native credential belongs to another origin');
    return false;
  }
  let access: z.infer<typeof AccessRecord> | null = null;
  try {
    const accessRaw = await getSecureItem(SECURE_KEYS.accessToken);
    access = accessRaw === null ? null : AccessRecord.parse(JSON.parse(accessRaw));
  } catch {
    access = null;
  }
  unpersisted = false;
  activate({
    workspaceId,
    origin: record.origin,
    clientId: record.clientId,
    tokenEndpoint: record.tokenEndpoint,
    revocationEndpoint: record.revocationEndpoint,
    refreshToken: record.refreshToken,
    refreshExpiresAt: record.refreshExpiresAt,
    deviceId: record.deviceId,
    accessToken: access?.accessToken ?? '',
    accessExpiresAt: access?.accessExpiresAt ?? 0,
  });
  return true;
}

/**
 * Server-side revoke of the device session stored in a workspace that is NOT
 * active (removing it from the switcher). Best effort: nothing stored, an
 * unreadable record or endpoints off the workspace origin revoke nothing.
 */
export async function revokeStoredSession(workspaceId: string, signal: AbortSignal): Promise<void> {
  const raw = await getSecureItemIn(workspaceId, SECURE_KEYS.refreshToken);
  if (raw === null) return;
  let record: z.infer<typeof RefreshRecord>;
  try {
    record = RefreshRecord.parse(JSON.parse(raw));
  } catch {
    return;
  }
  if (!isSameOrigin(record.revocationEndpoint, record.origin)) return;
  await deps.revoke(
    record.origin,
    record.revocationEndpoint,
    { token: record.refreshToken, clientId: record.clientId },
    { signal },
  );
}

/** What a check-in of a workspace that is not active found out. */
export type StoredCheckIn =
  /** The device session is alive; its rotated pair is stored in the slot (or parked). */
  | { kind: 'ok'; origin: string; policy: Record<string, unknown> | undefined }
  /** The server revoked, expired or deactivated the device session: wipe the workspace. */
  | { kind: 'device_revoked' }
  /** The refresh token or the client was refused: the workspace's sign-in is dead. */
  | { kind: 'rejected' }
  /** Nothing usable to check (no native credential, active, already under way). */
  | { kind: 'skipped' }
  /** No answer (offline, 5xx, 426): nothing changes. */
  | { kind: 'unavailable' };

/**
 * Check-in of a workspace that is NOT active (WS-SEC-1): its stored device
 * session is refreshed once, as a refresh of the active one would be, so a
 * revocation, a deactivated client or a new client policy reaches a workspace
 * that is only put aside. The rotated pair is stored into the slot (refresh
 * record first) unless its wipe has begun, or parked for its next restore when
 * the keychain refuses it; opening the workspace meanwhile waits for the
 * check-in, like for a refresh put aside by a switch. Lost answers are
 * re-presented inside the server's re-delivery window. Never throws.
 */
export function checkInStoredSession(workspaceId: string): Promise<StoredCheckIn> {
  if (boundWorkspace() === workspaceId || detachedRefreshes.has(workspaceId) || parked.has(workspaceId)) {
    return Promise.resolve({ kind: 'skipped' });
  }
  const run = runStoredCheckIn(workspaceId).catch((error: unknown): StoredCheckIn => {
    log.info('workspace check-in failed', { error });
    return { kind: 'unavailable' };
  });
  detachedRefreshes.set(workspaceId, run);
  void run.finally(() => {
    if (detachedRefreshes.get(workspaceId) === run) detachedRefreshes.delete(workspaceId);
  });
  return run;
}

async function runStoredCheckIn(workspaceId: string): Promise<StoredCheckIn> {
  if ((await getSecureItemIn(workspaceId, SECURE_KEYS.credentialKind)) !== 'oauth') return { kind: 'skipped' };
  const raw = await getSecureItemIn(workspaceId, SECURE_KEYS.refreshToken);
  if (raw === null) return { kind: 'skipped' };
  let record: z.infer<typeof RefreshRecord>;
  try {
    record = RefreshRecord.parse(JSON.parse(raw));
  } catch {
    return { kind: 'skipped' };
  }
  const slotOrigin = await getSecureItemIn(workspaceId, SECURE_KEYS.serverOrigin);
  if (
    record.origin !== slotOrigin ||
    !isSameOrigin(record.tokenEndpoint, record.origin) ||
    !isSameOrigin(record.revocationEndpoint, record.origin)
  ) {
    return { kind: 'skipped' };
  }
  const request = { refreshToken: record.refreshToken, clientId: record.clientId, clientVersion: deps.clientVersion() };
  const startedAt = deps.now();
  let outcome: TokenOutcome = await deps.refresh(record.origin, record.tokenEndpoint, request);
  for (
    let attempt = 1;
    (outcome.kind === 'network' || outcome.kind === 'unavailable') && attempt < REDELIVERY_MAX_ATTEMPTS;
    attempt++
  ) {
    const remaining = REDELIVERY_BUDGET_MS - (deps.now() - startedAt);
    const wanted = (outcome.kind === 'unavailable' ? outcome.retryAfterMs : undefined) ?? REDELIVERY_BASE_DELAY_MS * attempt;
    if (remaining <= wanted) break;
    await deps.sleep(wanted);
    outcome = await deps.refresh(record.origin, record.tokenEndpoint, request);
  }
  switch (outcome.kind) {
    case 'ok': {
      const set = tokenSetFrom(outcome.response, outcome.receivedAt, {
        workspaceId,
        origin: record.origin,
        clientId: record.clientId,
        tokenEndpoint: record.tokenEndpoint,
        revocationEndpoint: record.revocationEndpoint,
      });
      let stored = false;
      for (let attempt = 0; attempt < 2 && !stored; attempt++) {
        try {
          stored = await tracked(
            (async () => {
              if ((await getSecureItemIn(workspaceId, SECURE_KEYS.wipePending)) !== null) return true; // dropped on purpose
              await writeTokens(set);
              return true;
            })(),
          );
        } catch (error) {
          log.warn('checked-in tokens could not be stored', { error, attempt });
        }
      }
      if (!stored) parked.set(workspaceId, set);
      return { kind: 'ok', origin: record.origin, policy: outcome.response.client_policy };
    }
    case 'device_revoked':
      return { kind: 'device_revoked' };
    case 'invalid_grant':
    case 'invalid_client':
      return { kind: 'rejected' };
    default:
      return { kind: 'unavailable' };
  }
}

export function getNativeTokenSet(): Readonly<NativeTokenSet> | null {
  return current;
}

/** Refreshes once; concurrent callers share the same promise. */
export function refreshNativeSession(): Promise<RefreshVerdict> {
  if (refreshing) return refreshing;
  const set = current;
  refreshingSet = set;
  const run: Promise<RefreshVerdict> = runRefresh().finally(() => {
    // A switch hands the singleton over: never clear a newer workspace's refresh.
    if (refreshing === run) {
      refreshing = null;
      refreshingSet = null;
    }
    if (set !== null) {
      detached.delete(set);
      if (detachedRefreshes.get(set.workspaceId) === run) detachedRefreshes.delete(set.workspaceId);
    }
  });
  refreshing = run;
  return run;
}

async function runRefresh(): Promise<RefreshVerdict> {
  const set = current;
  if (set === null || getSession().ending) return 'rejected';
  const request = { refreshToken: set.refreshToken, clientId: set.clientId, clientVersion: deps.clientVersion() };
  const startedAt = deps.now();

  let outcome: TokenOutcome = await deps.refresh(set.origin, set.tokenEndpoint, request);
  for (
    let attempt = 1;
    (outcome.kind === 'network' || outcome.kind === 'unavailable') && attempt < REDELIVERY_MAX_ATTEMPTS;
    attempt++
  ) {
    const remaining = REDELIVERY_BUDGET_MS - (deps.now() - startedAt);
    const wanted = (outcome.kind === 'unavailable' ? outcome.retryAfterMs : undefined) ?? REDELIVERY_BASE_DELAY_MS * attempt;
    if (remaining <= wanted) break;
    await deps.sleep(wanted);
    // Put aside by a switch: keep re-presenting, the answer still belongs to its slot.
    if (!alive(set)) break;
    // The SAME token: if the first answer was lost after the server rotated,
    // this lands in the re-delivery window and returns the same new pair.
    outcome = await deps.refresh(set.origin, set.tokenEndpoint, request);
  }

  // Signed out, another sign-in replaced the session, or the workspace was
  // put aside for another one. A rotation still belongs to its own slot.
  if (current !== set) {
    if (outcome.kind === 'ok' && detached.has(set)) {
      await storeDetached(tokenSetFrom(outcome.response, outcome.receivedAt, set), set);
    }
    return current === null ? 'rejected' : 'refreshed';
  }

  switch (outcome.kind) {
    case 'ok': {
      const next = tokenSetFrom(outcome.response, outcome.receivedAt, set);
      const stored = await persistWithRetry(next, set);
      if (current !== set) {
        // Put aside while the write ran: a failed write is parked, not lost.
        if (!stored && detached.has(set)) await storeDetached(next, set);
        return current === null ? 'rejected' : 'refreshed';
      }
      unpersisted = !stored;
      activate(next);
      notifyGrant(outcome.response);
      return 'refreshed';
    }
    case 'device_revoked':
      log.warn('token endpoint revoked this device; wiping');
      void endSession({ reason: 'device_revoked' });
      return 'device_revoked';
    case 'invalid_grant':
    case 'invalid_client':
      log.warn('token endpoint refused the refresh', { outcome: outcome.kind });
      void endSession({ reason: 'credential_rejected' });
      return 'rejected';
    case 'upgrade_required':
      return 'upgrade_required';
    default:
      // network, unavailable, or an answer we could not read: keep the
      // current pair (the server's grace window covers a lost rotation).
      return 'unavailable';
  }
}

const refresher: CredentialRefresher = {
  async ensureFresh() {
    const set = current;
    if (set === null) return { ok: false, verdict: 'rejected' };
    if (unpersisted) unpersisted = !(await persistWithRetry(set, set));
    if (set.accessToken !== '' && set.accessExpiresAt - deps.now() > ACCESS_REFRESH_SKEW_MS) {
      return { ok: true, token: set.accessToken };
    }
    const verdict = await refreshNativeSession();
    const token = getBearerToken();
    if (verdict === 'refreshed' && current !== null && token) return { ok: true, token };
    return { ok: false, verdict: verdict === 'refreshed' ? 'rejected' : verdict };
  },
  async afterRejection(rejectedToken) {
    if (current === null) return 'rejected';
    // Someone already rotated past the rejected token.
    if (current.accessToken !== rejectedToken && current.accessToken !== '') return 'refreshed';
    return refreshNativeSession();
  },
};

registerWipeHook('native_session', async () => {
  const was = current;
  const flush = unpersisted;
  current = null;
  unpersisted = false;
  unregisterRevoke?.();
  unregisterRevoke = null;
  setCredentialRefresher(null);
  if (was !== null && isSuspendingWorkspace()) {
    // A switch, not a wipe: the workspace keeps its credential. A pair held
    // only in memory goes into its own slot, and a refresh still under way
    // stores its answer there too.
    detached.add(was);
    const inflight = refreshingSet === was ? refreshing : null;
    if (inflight !== null) {
      // The next workspace must not join this refresh (it would get "refreshed"
      // back while holding its own stale token); opening this one again waits for it.
      detachedRefreshes.set(was.workspaceId, inflight);
      refreshing = null;
      refreshingSet = null;
    }
    if (flush) await storeDetached(was, was);
    if (inflight === null) detached.delete(was);
  }
  await waitForWrites();
});

/**
 * A workspace being wiped (active or not): forget anything of it kept in
 * memory and let a token write already under way for it land BEFORE its
 * secure items are deleted.
 */
registerStoredWipeHook('native_session', async (workspaceId) => {
  parked.delete(workspaceId);
  detachedRefreshes.delete(workspaceId);
  for (const set of [...detached]) if (set.workspaceId === workspaceId) detached.delete(set);
  await waitForWrites();
});

/** Resolves when `work` settles or after `ms`, whichever is first. Never rejects. */
async function waitAtMost(work: Promise<unknown>, ms: number): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | null = null;
  const deadline = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, ms);
  });
  await Promise.race([work.then(noop, noop), deadline]);
  if (timer !== null) clearTimeout(timer);
}

function noop(): void {}

/**
 * With `current` cleared no new write starts; let the ones under way land
 * before the keys are deleted (bounded: a wipe never hangs on a keychain that
 * stopped answering). A write that settles may be followed by one it started
 * (belt and braces for persistWithRetry's own check), so wait until none is
 * left.
 */
async function waitForWrites(): Promise<void> {
  if (pendingWrites.size === 0) return;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let timedOut = false;
  const deadline = new Promise<void>((resolve) => {
    timer = setTimeout(() => {
      timedOut = true;
      resolve();
    }, WIPE_WRITE_WAIT_MS);
  });
  while (pendingWrites.size > 0 && !timedOut) {
    await Promise.race([Promise.all([...pendingWrites]), deadline]);
    // Let a write chained on a settled one register before checking again.
    await Promise.resolve();
  }
  if (timer !== null) clearTimeout(timer);
}

/** The client id this app registers as (its bundle id). */
export function nativeClientId(): string {
  return identity.bundleId;
}

/** Test helpers. */
export function __setNativeSessionDepsForTests(overrides: Partial<NativeSessionDeps>): void {
  deps = { ...defaultDeps, ...overrides };
}

export function __resetNativeSessionForTests(): void {
  pendingWrites.clear();
  deps = defaultDeps;
  current = null;
  unpersisted = false;
  refreshing = null;
  refreshingSet = null;
  detached.clear();
  parked.clear();
  detachedRefreshes.clear();
  unregisterRevoke?.();
  unregisterRevoke = null;
  setCredentialRefresher(null);
}

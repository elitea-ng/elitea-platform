import { z } from 'zod';

import { getClientVersion } from '@/api/clientVersion';
import { isSameOrigin } from '@/api/paths';
import { setCredentialRefresher, type CredentialRefresher, type RefreshVerdict } from '@/auth/credentialRefresh';
import { refreshTokens, revokeToken, type TokenOutcome, type TokenResponse } from '@/auth/tokenEndpoint';
import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import { endSession, registerRevokeHook, registerWipeHook } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import { getBearerToken, getSession, setCredential } from '@/state/session';

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
const REDELIVERY_BASE_DELAY_MS = 1_000;

export interface NativeEndpoints {
  tokenEndpoint: string;
  revocationEndpoint: string;
}

export interface NativeTokenSet extends NativeEndpoints {
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
  base: Pick<NativeTokenSet, 'origin' | 'clientId' | 'tokenEndpoint' | 'revocationEndpoint'>,
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
  await setSecureItem(
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
  await setSecureItem(
    SECURE_KEYS.accessToken,
    JSON.stringify({ v: 1, accessToken: set.accessToken, accessExpiresAt: set.accessExpiresAt }),
  );
}

/**
 * Stores `set`, retrying once. `owner` is the session the write belongs to:
 * once it is no longer current (a wipe cleared it, or another sign-in replaced
 * it) no further attempt starts, so a retry can never land after the wipe
 * hook stopped waiting and endSession deleted the keys.
 */
async function persistWithRetry(set: NativeTokenSet, owner: NativeTokenSet): Promise<boolean> {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (current !== owner) return false;
    try {
      await persistTokens(set);
      return true;
    } catch (error) {
      log.warn('rotated tokens could not be stored', { error, attempt });
    }
  }
  return false;
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
  const set = tokenSetFrom(response, receivedAt, base);
  await persistTokens(set);
  await tracked(setSecureItem(SECURE_KEYS.credentialKind, 'oauth'));
  unpersisted = false;
  activate(set);
  notifyGrant(response);
}

/**
 * Launch: restores the stored native credential for `origin`. Returns false
 * when there is none usable (the caller then signs in again). The endpoints
 * must still be on the workspace origin. A missing or unreadable access
 * record only means the first request refreshes.
 */
export async function restoreNativeSession(origin: string): Promise<boolean> {
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
  activate({
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

export function getNativeTokenSet(): Readonly<NativeTokenSet> | null {
  return current;
}

/** Refreshes once; concurrent callers share the same promise. */
export function refreshNativeSession(): Promise<RefreshVerdict> {
  if (refreshing) return refreshing;
  refreshing = runRefresh().finally(() => {
    refreshing = null;
  });
  return refreshing;
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
    if (current !== set) break;
    // The SAME token: if the first answer was lost after the server rotated,
    // this lands in the re-delivery window and returns the same new pair.
    outcome = await deps.refresh(set.origin, set.tokenEndpoint, request);
  }

  // Signed out, or another sign-in replaced the session meanwhile.
  if (current !== set) return current === null ? 'rejected' : 'refreshed';

  switch (outcome.kind) {
    case 'ok': {
      const next = tokenSetFrom(outcome.response, outcome.receivedAt, set);
      unpersisted = !(await persistWithRetry(next, set));
      if (current !== set) return current === null ? 'rejected' : 'refreshed';
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
  current = null;
  unpersisted = false;
  unregisterRevoke?.();
  unregisterRevoke = null;
  setCredentialRefresher(null);
  // With `current` cleared no new write starts; let the ones under way land
  // before endSession deletes the keys (bounded: a wipe never hangs on a
  // keychain that stopped answering).
  // A write that settles may be followed by one it started (belt and braces
  // for persistWithRetry's own check), so wait until none is left.
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
});

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
  unregisterRevoke?.();
  unregisterRevoke = null;
  setCredentialRefresher(null);
}

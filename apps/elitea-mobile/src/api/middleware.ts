import { Platform } from 'react-native';
import { z } from 'zod';

import { CLIENT_VERSION_HEADER, getClientVersion } from '@/api/clientVersion';
import { ApiError } from '@/api/errors';
import { isSameOrigin } from '@/api/paths';
import { registerAbortable } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { getCredentialRefresher, type RefreshVerdict } from '@/auth/credentialRefresh';
import {
  endSession as defaultEndSession,
  isEndingSession,
  isSuspendingWorkspace,
  markWorkspaceRevoked,
} from '@/security/endSession';
import { getBearerToken, getSession, setSession } from '@/state/session';

/**
 * The guarded fetch every REST call goes through (it is the `fetch` the
 * generated openapi-fetch client is built with):
 *
 *  1. Origin guard: the request must target this client's origin, and for an
 *     authenticated client also the session's origin; otherwise it throws
 *     `origin_mismatch` and the token is never attached.
 *  2. Headers: `X-Client-Version` and `Accept: application/json` on every
 *     request; `Authorization: Bearer …` only for authenticated clients.
 *  3. A 20 s deadline composed with the caller's signal, registered with the
 *     abort registry so `endSession` can cancel it.
 *  4. Response classification: an answer whose final URL is on another origin
 *     (a followed redirect) is rejected unread; 401 ends the session
 *     (`device_revoked` or `credential_rejected`); 426 raises the blocking
 *     "Update Agent Zefir" state without wiping anything (or locking); a
 *     later 2xx for a signed-in request re-checks the minimum version.
 *  5. Native sign-in: the access token is refreshed before it expires, and a
 *     401 for it (other than `device_revoked`) refreshes once (single-flight)
 *     and replays the request once with the new token.
 *
 * Plain non-2xx answers are returned untouched; `unwrap()` turns them into
 * `ApiError('http')`.
 */

export const DEFAULT_TIMEOUT_MS = 20_000;

export type AuthMode =
  /** The signed-in session's credential; 401 ends the session. */
  | { kind: 'session' }
  /** An explicit token being validated (developer sign-in); 401 is returned to the caller. */
  | { kind: 'token'; token: string }
  /** No credential at all (discovery, brand pack, token endpoint). */
  | { kind: 'anonymous' };

export type BaseFetch = (input: Request, init?: RequestInit) => Promise<Response>;

export interface GuardedFetchOptions {
  /** The workspace origin this client may talk to. */
  origin: string;
  auth: AuthMode;
  /** Transport; defaults to the global fetch (see plan F11: REST uses global fetch). */
  fetch?: BaseFetch;
  timeoutMs?: number;
  /** Injectable for tests. */
  endSession?: typeof defaultEndSession;
  /** Injectable for tests; defaults to `Platform.OS`. */
  platform?: string;
}

/**
 * No response is kept by an HTTP cache (DEF-R2): every answer is per-user,
 * and the token endpoint's carries the credential itself.
 *
 * Browsers honour `cache: 'no-store'`. React Native's fetch (whatwg-fetch over
 * the native networking module) does not pass `cache` to the platform at all;
 * for a GET it only appends a `_=<time>` cache-buster to the URL, which stores
 * just as much and changes the request. The HTTP cache is therefore turned off
 * natively instead (plugins/withNoHttpCache.js): on iOS the shared URLCache
 * gets zero capacity, on Android the OkHttp client (React Native's networking
 * module and expo/fetch) is built without its disk cache. The session wipe
 * also deletes the caches directory (src/security/cacheWipe.ts).
 */
export function noStoreInit(platform: string = Platform.OS): Pick<RequestInit, 'cache'> {
  return platform === 'web' ? { cache: 'no-store' } : {};
}

const errorBody = z.looseObject({ error: z.string().max(200).optional() });
const upgradeBody = z.looseObject({
  min_client_version: z.string().max(64).optional(),
  min_version: z.string().max(64).optional(),
});

const globalFetch: BaseFetch = (input, init) => globalThis.fetch(input, init);

export function createGuardedFetch(options: GuardedFetchOptions): (input: Request) => Promise<Response> {
  const { origin, auth } = options;
  const baseFetch = options.fetch ?? globalFetch;
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const endSession = options.endSession ?? defaultEndSession;
  const cacheInit = noStoreInit(options.platform);

  return async function guardedFetch(input: Request): Promise<Response> {
    const request = input;

    // 1. Origin guard, before any credential is read.
    const sessionOrigin = getSession().serverOrigin;
    if (!isSameOrigin(request.url, origin)) {
      throw new ApiError('origin_mismatch', 'request targets another origin');
    }
    if (auth.kind !== 'anonymous' && (sessionOrigin === null || !isSameOrigin(request.url, sessionOrigin))) {
      throw new ApiError('origin_mismatch', 'request targets an origin other than the workspace');
    }

    // The workspace this request is for: an answer that lands after a switch
    // must not change the session of the workspace switched to.
    const workspaceId = getSession().workspaceId;
    let token: string | null = null;
    if (auth.kind === 'session') {
      if (getSession().ending || isEndingSession()) {
        throw new ApiError('session_ending', 'signing out');
      }
      // Synchronous for a PAT (no await before the request is registered
      // with the abort registry); a native token may be refreshed first.
      const pending = sessionBearerToken();
      token = typeof pending === 'string' ? pending : await pending;
    } else if (auth.kind === 'token') {
      token = auth.token;
    }

    // A session request may be replayed once after a token refresh.
    const replay = auth.kind === 'session' ? request.clone() : null;
    let response = await send(request, token);
    let failure = await classify(response, token, workspaceId);
    if (failure && replay && shouldReplay(failure)) {
      const renewed = getBearerToken();
      // Only the same workspace's renewed token (never one of another workspace switched to meanwhile).
      const sameWorkspace = getSession().serverOrigin !== null && isSameOrigin(request.url, getSession().serverOrigin!);
      if (renewed !== null && renewed !== token && sameWorkspace) {
        response = await send(replay, renewed);
        failure = await classify(response, renewed, workspaceId);
      }
    }
    if (failure) throw failure;
    return response;
  };

  async function send(request: Request, token: string | null): Promise<Response> {
    // 2. Headers.
    const headers = new Headers(request.headers);
    headers.set(CLIENT_VERSION_HEADER, getClientVersion());
    if (!headers.has('Accept')) headers.set('Accept', 'application/json');
    if (token !== null) headers.set('Authorization', `Bearer ${token}`);
    else headers.delete('Authorization');

    // 3. Deadline + caller signal + abort registry.
    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, timeoutMs);
    const callerSignal = request.signal as AbortSignal | undefined;
    const onCallerAbort = () => controller.abort();
    if (callerSignal?.aborted) controller.abort();
    else callerSignal?.addEventListener?.('abort', onCallerAbort);
    const unregister = registerAbortable(controller);

    let response: Response;
    try {
      response = await baseFetch(request, { headers, signal: controller.signal, ...cacheInit });
    } catch (error) {
      if (timedOut) throw new ApiError('timeout', 'request timed out', { cause: error });
      if (controller.signal.aborted) throw new ApiError('aborted', 'request cancelled', { cause: error });
      throw new ApiError('network', 'network request failed', { cause: error });
    } finally {
      clearTimeout(timer);
      unregister();
      callerSignal?.removeEventListener?.('abort', onCallerAbort);
    }

    // 4. Classification (redirect check here, session meaning in classify()).
    if (response.url && !isSameOrigin(response.url, origin)) {
      throw new ApiError('origin_mismatch', 'response was redirected to another origin', {
        status: response.status,
      });
    }
    return response;
  }

  function classify(response: Response, token: string | null, workspaceId: string | null): Promise<ApiError | null> {
    return classifySessionFailure(response, { token, sessionAuth: auth.kind === 'session', endSession, workspaceId });
  }
}

/** A 401 that a native token refresh has already answered: replay once. */
export function shouldReplay(failure: ApiError): boolean {
  if (failure.kind !== 'session_invalid') return false;
  if (failure.reason === 'refreshed') return true;
  return failure.reason === 'stale_credential' && getSession().credential?.kind === 'oauth';
}

/** The error a request fails with when the native credential could not be renewed. */
export function refreshFailure(verdict: Exclude<RefreshVerdict, 'refreshed'>, status?: number): ApiError {
  switch (verdict) {
    case 'device_revoked':
      return new ApiError('session_invalid', 'device revoked', { status, code: 'device_revoked', reason: 'device_revoked' });
    case 'rejected':
      return new ApiError('session_invalid', 'credential rejected', { status, reason: 'credential_rejected' });
    case 'upgrade_required':
      return new ApiError('upgrade_required', 'this app version is no longer supported', { status: 426 });
    case 'unavailable':
      return new ApiError('network', 'could not renew the sign-in', { status });
  }
}

/**
 * The bearer token for a session request: the PAT as stored, or a native
 * access token that is refreshed first when it is (about to be) expired.
 */
export function sessionBearerToken(): string | Promise<string> {
  const refresher = getCredentialRefresher();
  if (getSession().credential?.kind === 'oauth' && refresher !== null) {
    // Several workspaces can be connected: if the active one changed while
    // the token was being renewed, the token now in the session belongs to
    // ANOTHER workspace and must never be sent to this request's origin.
    const workspace = getSession().workspaceId;
    return refresher.ensureFresh().then((fresh) => {
      if (getSession().workspaceId !== workspace) {
        throw new ApiError('origin_mismatch', 'the workspace changed while the sign-in was renewed');
      }
      if (fresh.ok) return fresh.token;
      throw refreshFailure(fresh.verdict);
    });
  }
  const token = getBearerToken();
  if (token === null) {
    throw new ApiError('session_invalid', 'not signed in', { reason: 'no_credential' });
  }
  return token;
}

/**
 * True when a response for `workspaceId` (undefined: not known, treated as
 * current) arrives while that workspace is no longer the active one, or while
 * the active one is being put aside or replaced.
 */
function answersAnotherWorkspace(workspaceId: string | null | undefined): boolean {
  if (workspaceId === undefined) return false;
  const session = getSession();
  return session.switching || isSuspendingWorkspace() || session.workspaceId !== workspaceId;
}

/**
 * The session-level meaning of a response, shared by the REST guarded fetch
 * and the event-stream client:
 *  - 401 for the session credential: `device_revoked` or `credential_rejected`,
 *    which ends the session (unless the token has since been replaced);
 *  - 426: the blocking "Update Agent Zefir" state, nothing wiped.
 * Returns the error to throw, or null when the response is not one of these.
 */
export async function classifySessionFailure(
  response: Pick<Response, 'status'> & { clone?: () => Pick<Response, 'json'>; json?: () => Promise<unknown> },
  context: {
    token: string | null;
    sessionAuth: boolean;
    endSession?: typeof defaultEndSession;
    /**
     * The workspace the request was sent for (captured before the send). When
     * given and no longer the active one, the answer changes no session state.
     */
    workspaceId?: string | null;
  },
): Promise<ApiError | null> {
  const endSession = context.endSession ?? defaultEndSession;
  const readBody = async (): Promise<unknown> => {
    try {
      if (response.clone) return await response.clone().json();
      if (response.json) return await response.json();
    } catch {
      // not JSON
    }
    return undefined;
  };

  if (response.status === 401 && context.sessionAuth) {
    const parsed = errorBody.safeParse(await readBody());
    const code = parsed.success ? parsed.data.error : undefined;
    if (getBearerToken() !== context.token) {
      // Answer to a credential that has since been replaced or removed.
      return new ApiError('session_invalid', 'credential changed', {
        status: 401,
        code,
        reason: 'stale_credential',
      });
    }
    if (answersAnotherWorkspace(context.workspaceId)) {
      // The workspace this answers is being put aside, or already is: ending
      // "the session" would end (or half-reset) the wrong one. A revocation
      // marks that workspace's slot for its wipe; anything else is stale.
      if (code === 'device_revoked' && context.workspaceId) await markWorkspaceRevoked(context.workspaceId);
      return new ApiError('session_invalid', 'answer for a workspace no longer in use', {
        status: 401,
        code,
        reason: 'stale_credential',
      });
    }
    if (code === 'device_revoked') {
      log.warn('server revoked this device; ending session');
      void endSession({ reason: 'device_revoked' });
      return new ApiError('session_invalid', 'device revoked', {
        status: 401,
        code,
        reason: 'device_revoked',
      });
    }
    // Native sign-in: an expired (not revoked) access token. Refresh once,
    // shared by every request that hit the same 401; the caller replays.
    const refresher = getCredentialRefresher();
    if (getSession().credential?.kind === 'oauth' && refresher !== null && context.token !== null) {
      const verdict = await refresher.afterRejection(context.token);
      if (verdict === 'refreshed') {
        return new ApiError('session_invalid', 'credential refreshed', { status: 401, code, reason: 'refreshed' });
      }
      return refreshFailure(verdict, 401);
    }
    log.warn('credential rejected by server; ending session', {
      kind: getSession().credential?.kind ?? 'none',
    });
    void endSession({ reason: 'credential_rejected' });
    return new ApiError('session_invalid', 'credential rejected', {
      status: 401,
      code,
      reason: 'credential_rejected',
    });
  }

  if (response.status === 426) {
    const parsed = upgradeBody.safeParse(await readBody());
    const minVersion = parsed.success
      ? (parsed.data.min_client_version ?? parsed.data.min_version)
      : undefined;
    // Only for the workspace still in use: a 426 that answers the previous
    // workspace's request after a switch is that workspace's version floor.
    if (!answersAnotherWorkspace(context.workspaceId)) {
      setSession({ upgradeRequired: minVersion === undefined ? {} : { minVersion } });
    }
    return new ApiError('upgrade_required', 'this app version is no longer supported', {
      status: 426,
    });
  }
  // The workspace answered a signed-in request normally while the update
  // screen is up: its minimum may have been lowered (DEF-R4). Ask the
  // discovery document, which decides; a 2xx alone does not clear the screen,
  // because not every route is behind the server's version gate.
  if (context.sessionAuth && response.status >= 200 && response.status < 300 && getSession().upgradeRequired !== null) {
    upgradeRecheck?.();
  }
  return null;
}

let upgradeRecheck: (() => void) | null = null;

/**
 * Sets what a successful signed-in request does while the update screen is up
 * (src/security/policyStore.ts: re-read the minimum version). Null clears it.
 */
export function setUpgradeRecheck(recheck: (() => void) | null): void {
  upgradeRecheck = recheck;
}

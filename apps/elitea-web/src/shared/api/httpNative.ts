/**
 * The native-client half of the HTTP core (ADR-0029 decision 9). `http.ts`
 * stays the only caller of `fetch` for the typed client; this module holds the
 * decisions a registered `NativeTransport` changes, as pure functions of
 * their inputs, so the browser path in `http.ts` reads as it always did.
 */
import type { NativeTransport } from './nativeTransport';
import { credentialRefusalCode, isDeviceRevoked, resourceAuthorizationBody } from './reauth-policy';

/**
 * The browser path's bearer: the DEV static token, only under
 * `import.meta.env.DEV`, which Vite replaces with `false` in production — that
 * branch cannot exist in a production bundle (V4 proves it with a bundle
 * grep). A native transport supplies its own token (`accessToken()`), through
 * the same `applyAuthHeaders` seam.
 */
export function devBearer(devToken: string | undefined): string | undefined {
  return import.meta.env.DEV && devToken !== undefined && devToken !== '' ? devToken : undefined;
}

/**
 * The bearer, or `undefined` when `url` is not on the deployment origin the
 * transport is bound to: the token must not travel anywhere else.
 */
export function bearerFor(native: NativeTransport | undefined, url: string, bearer: string | undefined): string | undefined {
  if (native?.origin === undefined || bearer === undefined) return bearer;
  try {
    return new URL(url).origin === new URL(native.origin).origin ? bearer : undefined;
  } catch {
    return undefined;
  }
}

/** Bearer + the transport's fixed headers (`X-Client-Version`). */
export function applyAuthHeaders(headers: Headers, bearer: string | undefined, native: NativeTransport | undefined): void {
  if (bearer !== undefined) headers.set('Authorization', `Bearer ${bearer}`);
  for (const [name, value] of Object.entries(native?.headers ?? {})) headers.set(name, value);
}

/**
 * The fetch init. A native client never sends or accepts cookies and never
 * follows a redirect with a bearer attached: the token must not travel to a
 * host the user did not choose. Without a transport the init is exactly the
 * browser's.
 */
export function requestInit(
  method: string,
  headers: Headers,
  credentials: RequestCredentials,
  native: NativeTransport | undefined,
): RequestInit {
  return native === undefined ? { method, headers, credentials } : { method, headers, credentials: 'omit', redirect: 'error' };
}

/** The failure shapes of `HttpFailure` that a native request can end in (kept structurally equal; `http.ts` returns them as they are). */
type NativeFailure =
  | { kind: 'http'; status: number; url: string; body: unknown }
  | { kind: 'auth'; status: number; url: string; refusal?: string | undefined }
  | { kind: 'network'; url: string; message: string; cause: unknown }
  | { kind: 'aborted'; url: string };

/** What `http.ts` does with the response of a native request. */
export type NativeAnswer = { kind: 'response'; response: Response } | { kind: 'failure'; failure: NativeFailure };

export interface NativeAttempt {
  native: NativeTransport;
  /** The response to the first send. */
  first: Response;
  /** The token that first send carried. */
  bearer: string | undefined;
  init: RequestInit;
  send: () => Promise<Response>;
  url: string;
  signal: AbortSignal | undefined;
}

/**
 * Judge the first response of a native request, in place of behaviours 2/2c/3
 * (which are about a cookie session and a popup, neither of which a native
 * client has).
 *
 * 401 → a resource-authorization body is kept (2b, as everywhere); a
 * `device_revoked` ends the session and asks the host to wipe; any other 401
 * asks the host to refresh ONCE and replays the identical request with the new
 * token. A refusal of that replay is returned as an auth failure WITHOUT
 * signing out (the refresh just proved the session alive; only `ended` from
 * the refresh, or `device_revoked`, ends it). 426 tells the host this build is
 * too old (the response is still returned).
 */
export async function judgeNativeResponse(attempt: NativeAttempt): Promise<NativeAnswer> {
  const { native, first } = attempt;
  if (first.status === 426) native.onUpgradeRequired?.();
  if (first.status !== 401) return { kind: 'response', response: first };

  const body = await resourceAuthorizationBody(first);
  if (body !== undefined) return resourceAuth(first, body);
  if (await isDeviceRevoked(first)) {
    native.signOut('device_revoked');
    return sessionEnded(first);
  }
  return replayAfterRefresh(attempt);
}

function resourceAuth(response: Response, body: unknown): NativeAnswer {
  return { kind: 'failure', failure: { kind: 'http', status: response.status, url: response.url, body } };
}

/** The session failure, carrying the server's own name for the refusal (#538). */
async function sessionEnded(response: Response): Promise<NativeAnswer> {
  const refusal = await credentialRefusalCode(response);
  return { kind: 'failure', failure: { kind: 'auth', status: response.status, url: response.url, refusal } };
}

async function replayAfterRefresh(attempt: NativeAttempt): Promise<NativeAnswer> {
  const { native, first, init } = attempt;
  const outcome = await native.refresh(attempt.bearer);
  if (outcome === 'unavailable') {
    const message = `http: request to ${attempt.url} failed: the session could not be renewed right now`;
    return { kind: 'failure', failure: { kind: 'network', url: attempt.url, message, cause: outcome } };
  }
  const fresh = outcome === 'refreshed' ? await native.accessToken() : undefined;
  if (fresh === undefined) {
    native.signOut('refresh_failed');
    return sessionEnded(first);
  }
  if (attempt.signal?.aborted === true) return { kind: 'failure', failure: { kind: 'aborted', url: attempt.url } };
  const scoped = bearerFor(native, attempt.url, fresh);
  if (scoped !== undefined) (init.headers as Headers).set('Authorization', `Bearer ${scoped}`);
  let response: Response;
  try {
    response = await attempt.send();
  } catch (cause) {
    const message = cause instanceof Error ? cause.message : String(cause);
    return { kind: 'failure', failure: { kind: 'network', url: attempt.url, message: `http: request to ${attempt.url} failed: ${message}`, cause } };
  }
  if (response.status !== 401) return { kind: 'response', response };
  if (await isDeviceRevoked(response)) native.signOut('device_revoked');
  const body = await resourceAuthorizationBody(response);
  return body !== undefined ? resourceAuth(response, body) : sessionEnded(response);
}

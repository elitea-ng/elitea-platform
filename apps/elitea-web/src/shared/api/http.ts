/**
 * HTTP core — the only place `fetch` is called (R-A1, spec §5.4).
 *
 * A FACTORY, not a singleton: `createHttpClient(cfg)` takes a narrow
 * `HttpConfig` so this unit (F4) stays decoupled from `shared/config`
 * (unit F3); the app layer (R2) wires `getConfig()` → factory.
 *
 * The nine §5.4 behaviours implemented here or in `auth/`:
 *  1. explicit `credentials`: 'same-origin', switching to 'include' when the
 *     resolved base URL is cross-origin (old `fetchBaseQuery` set none).
 *  2. real 401 → re-auth; redirect-sniff kept as SECONDARY signal (old app had
 *     redirect-sniff only — eliteaApi.js:21-49; the genuine 401 from elitea-main
 *     middleware/auth.go:155 surfaced as a plain error). 403 is DELIBERATELY not
 *     a re-auth signal — see `needsReauth`; nor is a 401 on a `background` poll.
 *  3. single-flight re-auth: N concurrent 401s → one re-auth flow.
 *  6. dev bearer ONLY under `import.meta.env.DEV` (statically eliminated from
 *     production bundles; old app leaked it via config — eliteaApi.js:60-63).
 *  8. AbortSignal on every request (react-query `signal` compatible).
 *  9. W3C `traceparent` when tracing is enabled (shape preserved from
 *     apps/elitea-ui/src/services/tracing/TraceService.js:55-61).
 *  §3.6: discriminated `Result` return — 4xx/5xx/network/abort are values,
 *     never throws; throws are reserved for programmer errors.
 *  Replayability: bodies are serialized once and reused, so the post-re-auth
 *     retry is byte-identical (old app retried a pre-cloned Request —
 *     eliteaApi.js:17-18).
 */

export interface HttpConfig {
  /** API base, relative (`/api/v2`) or absolute (`https://api.example/api/v2`). */
  baseUrl: string;
  /** Explicit override; when omitted it is derived from `baseUrl` vs the page origin. */
  credentialsMode?: RequestCredentials;
  tracingEnabled?: boolean;
  /** Attached as `Authorization: Bearer …` ONLY under `import.meta.env.DEV`. */
  devToken?: string;
  /**
   * Re-auth flow (auth/popup.ts): resolves once the session is restored,
   * rejects when re-auth failed. Omit for clients that must never trigger
   * re-auth (e.g. the callback page's session probe).
   */
  reauthenticate?: () => Promise<void>;
  /**
   * Called when the server states that this browser's session has EXPIRED,
   * with the login URL it named.
   *
   * ONLY THE APP SHELL'S OWN SESSION PROBE SUPPLIES IT, and that is the whole
   * design. `/forward-auth/info` answers `401 {"error":{"code":
   * "session_expired"}}` when a cookie was presented and no longer works
   * (services/elitea-main/docs/browser-sessions.md). That answer is about the
   * SESSION, so acting on it — a full-page navigation to the identity
   * provider — is correct.
   *
   * A 401 from the notification bell, a permission read or any other
   * peripheral call is NOT that answer, and must never move the browser. A
   * peripheral 401 once re-authenticated in a loop forever; `background: true`
   * exists for the same reason. Clients that do not configure this handler
   * keep the existing behaviour exactly.
   */
  onSessionExpired?: (loginUrl: string) => void;
}

/** @public Wave-1 surface: consumed by S4/S6 endpoint modules and R2. */
export type HttpFailure =
  | { kind: 'http'; status: number; url: string; body: unknown }
  | { kind: 'auth'; status: number; url: string }
  | { kind: 'network'; url: string; message: string; cause: unknown }
  | { kind: 'aborted'; url: string };

/**
 * @public Wave-1 surface: consumed by S4/S6 endpoint modules and R2.
 * `headers` is the real `Response.headers` (not re-derived/synthesised) —
 * `eliteaFetch` (S4's mutator) needs it verbatim to build the
 * `{data, status, headers}` envelope orval's generated types declare for
 * every operation (`includeHttpResponseReturnType` defaults to `true` in
 * orval 8.23.0's `@orval/fetch` generator, confirmed by reading
 * `node_modules/@orval/fetch/dist/index.mjs`; `orval.config.ts` does not
 * override it). With a custom mutator configured, orval's own generated
 * code never builds this envelope itself — it fully delegates to the
 * mutator (`return eliteaFetch<T>(url, options)`, no wrapping) — so the
 * envelope has to be constructed somewhere on this side of the boundary,
 * and `headers` only exists on the raw `Response`, not before this point.
 */
export type HttpResult<T> =
  | { ok: true; status: number; data: T; headers: Headers }
  | { ok: false; error: HttpFailure };

/** @public Wave-1 surface: consumed by S4/S6 endpoint modules and R2. */
export type HttpMethod = 'GET' | 'HEAD' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

/** @public Wave-1 surface: consumed by S4/S6 endpoint modules and R2. */
export interface HttpRequestOptions {
  /** react-query passes its per-query `signal` straight through here (§5.4). */
  signal?: AbortSignal;
  headers?: Readonly<Record<string, string>>;
  /** JSON-serialized once (replayable); pass a string to send it verbatim. `FormData`/`Blob`/`URLSearchParams`/`ArrayBuffer` also pass through unchanged (their own Content-Type, e.g. multipart, is left for `fetch` to set). */
  body?: unknown;
  query?: Readonly<Record<string, string | number | boolean | undefined>>;
  /** A peripheral poll whose 401 must NOT escalate to re-auth — see `features/notifications/api/notifications.ts` for the logout loop this exists to stop. */
  background?: boolean;
}

export interface HttpClient {
  readonly baseUrl: string;
  readonly credentials: RequestCredentials;
  /**
   * True when this client escalates a 401 into the re-auth flow. Callers
   * that must NOT trigger re-auth — the callback page's session probe above
   * all — assert on it (see auth/verify-session.ts).
   */
  readonly reauthConfigured: boolean;
  request<T>(method: HttpMethod, path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
  get<T>(path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
  post<T>(path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
  put<T>(path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
  patch<T>(path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
  delete<T>(path: string, options?: HttpRequestOptions): Promise<HttpResult<T>>;
}

import {
  needsReauth,
  resourceAuthorizationBody,
  sessionExpiredLoginUrl,
} from './reauth-policy';

/* ── behaviour 1: credentials resolution ─────────────────────────────────── */

/**
 * 'same-origin' explicitly, 'include' when the resolved base URL is
 * cross-origin — pointing the API at another origin must not silently drop
 * the session cookie (§5.4).
 */
export function resolveCredentialsMode(baseUrl: string, pageOrigin: string): RequestCredentials {
  const resolved = new URL(baseUrl, pageOrigin);
  return resolved.origin === new URL(pageOrigin).origin ? 'same-origin' : 'include';
}

/* ── behaviour 9: W3C traceparent (shape from TraceService.js:55-61) ─────── */

function randomHex(chars: number): string {
  const bytes = new Uint8Array(chars / 2);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/** `{version}-{trace-id}-{span-id}-{trace-flags}`, e.g. `00-…32…-…16…-01`. */
export function generateTraceparent(): string {
  return `00-${randomHex(32)}-${randomHex(16)}-01`;
}

/* ── request assembly ────────────────────────────────────────────────────── */

function buildUrl(base: string, path: string, query?: HttpRequestOptions['query']): string {
  const url = new URL(base.replace(/\/$/, '') + (path.startsWith('/') ? path : `/${path}`));
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value !== undefined) url.searchParams.set(key, String(value));
  }
  return url.toString();
}

/** True for the `BodyInit` variants `fetch` already knows how to send verbatim, with their own Content-Type — JSON-stringifying any of these silently discards the payload (`JSON.stringify(new FormData())` is `"{}"`, no throw). */
function isPreEncodedBody(body: unknown): body is FormData | Blob | URLSearchParams | ArrayBuffer {
  return body instanceof FormData || body instanceof Blob || body instanceof URLSearchParams || body instanceof ArrayBuffer;
}

/**
 * BUG FIX, found while porting Wave-2 unit C1 (chat model/store): this used
 * to `JSON.stringify` every non-string body unconditionally, including
 * `FormData` — `JSON.stringify(new FormData())` returns `"{}"` (FormData has
 * no enumerable own properties), so a multipart upload's real payload was
 * silently replaced with an empty JSON object and no error was ever thrown.
 * Reproduced live: `shared/api/generated/artifacts/artifacts.ts`'s
 * `createArtifact` and `shared/api/generated/applications/applications.ts`'s
 * `uploadApplicationIcon` both build a `FormData` and pass it straight into
 * `eliteaFetch({ body: formData })` — both were silently sending an empty
 * body to the server. `FormData`/`Blob`/`URLSearchParams`/`ArrayBuffer` are
 * passed through unchanged now; `prepare()` below also stops forcing
 * `Content-Type: application/json` onto them, so `fetch` sets its own
 * (for `FormData`, `multipart/form-data` with the correct boundary).
 */
function serializeBody(method: HttpMethod, body: unknown, url: string): string | FormData | Blob | URLSearchParams | ArrayBuffer | undefined {
  if (body === undefined) return undefined;
  if (method === 'GET' || method === 'HEAD') {
    throw new TypeError(`http: ${method} ${url} cannot carry a request body`);
  }
  if (typeof body === 'string') return body;
  if (isPreEncodedBody(body)) return body;
  try {
    return JSON.stringify(body);
  } catch (cause) {
    // Programmer error — rethrown with context (§3.6).
    throw new TypeError(`http: request body for ${method} ${url} is not JSON-serializable`, { cause });
  }
}

interface PreparedRequest {
  url: string;
  init: RequestInit;
}

function prepare(cfg: HttpConfig, credentials: RequestCredentials, method: HttpMethod, path: string, options: HttpRequestOptions): PreparedRequest {
  const url = buildUrl(new URL(cfg.baseUrl, window.location.origin).toString(), path, options.query);
  const headers = new Headers(options.headers);
  const body = serializeBody(method, options.body, url);
  if (body !== undefined && typeof options.body !== 'string' && !isPreEncodedBody(options.body) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (cfg.tracingEnabled === true) headers.set('traceparent', generateTraceparent());
  // Behaviour 6: statically eliminated from production bundles — Vite
  // replaces `import.meta.env.DEV` with `false` there, so no dev-token code
  // path can exist outside dev builds (V4 proves this with a bundle grep).
  if (import.meta.env.DEV) {
    if (cfg.devToken !== undefined && cfg.devToken !== '') {
      headers.set('Authorization', `Bearer ${cfg.devToken}`);
    }
    headers.set('Cache-Control', 'no-cache'); // parity: eliteaApi.js:63
  }
  const init: RequestInit = { method, headers, credentials };
  if (body !== undefined) init.body = body;
  if (options.signal !== undefined) init.signal = options.signal;
  return { url, init };
}

/* ── Result construction (§3.6: errors are values at the boundary) ───────── */

function isAbortError(cause: unknown): boolean {
  // Realm-safe: under vitest/undici the DOMException comes from another
  // realm, so `instanceof DOMException` would misclassify aborts as network
  // failures. The name is the cross-realm contract.
  return typeof cause === 'object' && cause !== null && (cause as { name?: unknown }).name === 'AbortError';
}

function failure<T>(error: HttpFailure): HttpResult<T> {
  return { ok: false, error };
}

function fromException<T>(cause: unknown, url: string): HttpResult<T> {
  if (isAbortError(cause)) return failure({ kind: 'aborted', url });
  const message = cause instanceof Error ? cause.message : String(cause);
  return failure({ kind: 'network', url, message: `http: request to ${url} failed: ${message}`, cause });
}

async function toResult<T>(response: Response): Promise<HttpResult<T>> {
  const text = await response.text();
  let body: unknown = text === '' ? undefined : text;
  if (text !== '' && (response.headers.get('content-type') ?? '').includes('application/json')) {
    try {
      body = JSON.parse(text);
    } catch {
      // Handled (§3.6): a server lying about content-type is not a crash —
      // the raw text is surfaced instead.
    }
  }
  if (!response.ok) {
    return failure({ kind: 'http', status: response.status, url: response.url, body });
  }
  return { ok: true, status: response.status, data: body as T, headers: response.headers };
}

/**
 * The refusal an EXPIRED session becomes, or `undefined` when this 401 is not
 * that answer.
 *
 * It is checked BEFORE the re-auth escalation because the two are different
 * answers: a re-auth popup is a recovery attempt, and this is the server
 * telling the shell there is nothing to recover. Only a client that configured
 * `onSessionExpired` reaches it, so no peripheral call can navigate.
 */
async function expiredSessionRefusal<T>(
  cfg: HttpConfig,
  response: Response,
): Promise<HttpResult<T> | undefined> {
  if (cfg.onSessionExpired === undefined) return undefined;
  const loginUrl = await sessionExpiredLoginUrl(response);
  if (loginUrl === undefined) return undefined;
  cfg.onSessionExpired(loginUrl);
  return failure({ kind: 'auth', status: response.status, url: response.url });
}

/**
 * The failure a 401 that survived re-authentication becomes.
 *
 * A resource-authorization 401 keeps its BODY, because `auth_metadata` appears
 * nowhere else; every other one is a session failure with no body to keep.
 */
async function authRefusal<T>(response: Response): Promise<HttpResult<T>> {
  const resourceAuth = await resourceAuthorizationBody(response);
  if (resourceAuth !== undefined) {
    return failure({ kind: 'http', status: response.status, url: response.url, body: resourceAuth });
  }
  return failure({ kind: 'auth', status: response.status, url: response.url });
}

/* ── factory ─────────────────────────────────────────────────────────────── */

export function createHttpClient(cfg: HttpConfig): HttpClient {
  if (typeof cfg.baseUrl !== 'string' || cfg.baseUrl === '') {
    throw new TypeError('http: HttpConfig.baseUrl is required'); // programmer error
  }
  const credentials = cfg.credentialsMode ?? resolveCredentialsMode(cfg.baseUrl, window.location.origin);

  // Behaviour 3: single-flight re-auth — client-scoped (no module-scope
  // singletons, R-S2), one popup for N concurrent 401s.
  let reauthInFlight: Promise<boolean> | null = null;
  const runReauth = (): Promise<boolean> => {
    const reauthenticate = cfg.reauthenticate;
    if (reauthenticate === undefined) return Promise.resolve(false);
    reauthInFlight ??= reauthenticate()
      .then(
        () => true,
        () => false, // handled (§3.6): a failed re-auth becomes a Result below
      )
      .finally(() => {
        reauthInFlight = null;
      });
    return reauthInFlight;
  };

  async function request<T>(method: HttpMethod, path: string, options: HttpRequestOptions = {}): Promise<HttpResult<T>> {
    const { url, init } = prepare(cfg, credentials, method, path, options);

    let response: Response;
    try {
      response = await fetch(url, init);
    } catch (cause) {
      return fromException<T>(cause, url);
    }

    // Behaviour 2c: the server stated that this SESSION expired.
    const expired = await expiredSessionRefusal<T>(cfg, response);
    if (expired !== undefined) return expired;

    if (needsReauth(response) && options.background !== true) {
      // Behaviour 2b first: a resource-authorization 401 is not a session
      // failure — surface its body instead of burning it on a re-auth.
      const resourceAuth = await resourceAuthorizationBody(response);
      if (resourceAuth !== undefined) {
        return failure({ kind: 'http', status: response.status, url: response.url, body: resourceAuth });
      }
      // Behaviour 2: a real 401 is the PRIMARY signal, redirect-sniff the
      // secondary one; both funnel into the same single-flight re-auth.
      const restored = await runReauth();
      if (!restored) return failure({ kind: 'auth', status: response.status, url: response.url });
      if (options.signal?.aborted === true) return failure({ kind: 'aborted', url });
      try {
        // Byte-identical replay: same serialized body, same headers.
        response = await fetch(url, init);
      } catch (cause) {
        return fromException<T>(cause, url);
      }
      if (needsReauth(response)) return authRefusal<T>(response);
    }

    return toResult<T>(response);
  }

  return {
    baseUrl: cfg.baseUrl,
    credentials,
    reauthConfigured: cfg.reauthenticate !== undefined,
    request,
    get: (path, options) => request('GET', path, options),
    post: (path, options) => request('POST', path, options),
    put: (path, options) => request('PUT', path, options),
    patch: (path, options) => request('PATCH', path, options),
    delete: (path, options) => request('DELETE', path, options),
  };
}

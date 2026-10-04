/**
 * mutator.ts — the `eliteaFetch` bridge orval's generated hooks call into
 * (unit S4; spec §5.3 `override.mutator: { path: 'src/shared/api/http.ts',
 * name: 'eliteaFetch' }`).
 *
 * F4's `src/shared/api/http.ts` does NOT export a function named
 * `eliteaFetch` — it exports `createHttpClient(cfg): HttpClient`, a FACTORY
 * with no module-scope singleton (R-S2: "No store may be created at module
 * scope in a file that is also imported by `app/`"). Two things follow:
 *
 *  1. This adapter cannot point `orval.config.ts`'s mutator directly at
 *     `http.ts` — there is no `eliteaFetch` export there to point at. This
 *     file supplies it, and `orval.config.ts`'s mutator path is repointed
 *     here instead (documented as a deliberate deviation from the spec's
 *     verbatim snippet in the S4 report).
 *  2. The one `HttpClient` instance every generated hook shares can only be
 *     *injected*, not constructed at this module's top level — generated
 *     hook modules are statically imported by feature code well before the
 *     app layer (R2) has resolved `shared/config`'s runtime config. So the
 *     client lives in a module-private variable, set once via
 *     `configureGeneratedClient(cfg)` from `app/` bootstrap, exactly the
 *     factory-then-inject shape R-S2 already requires elsewhere.
 *
 * Call-signature contract (verified via context7 against orval 8.23.0's
 * `@orval/fetch` generator, NOT guessed — orval's `httpClient: 'fetch'`
 * output for a custom mutator calls it as a plain two-argument function,
 * `mutator<T>(url: string, options: RequestInit): Promise<T>`, with the
 * FULL relative URL (including any query string) as the first argument and
 * an already-JSON-stringified `body` + `Content-Type` header already set
 * when there is one. This is a different shape from the single
 * `{url, method, params, data, headers, signal}` config-object convention
 * orval uses for its axios mutators — mixing the two up is the most likely
 * way to get this file wrong.) Because `orval.config.ts` sets
 * `baseUrl: ''`, the `url` this function receives is always
 * server-relative (e.g. `/elitea_core/applications/prompt_lib/{projectId}`
 * with real values interpolated), which is exactly the `path` shape
 * `HttpClient.request` expects — `http.ts` itself resolves the real
 * `/api/v2*` base.
 */
import { createHttpClient } from '../http';
import type { HttpClient, HttpConfig, HttpFailure, HttpMethod } from '../http';

const HTTP_METHODS: readonly HttpMethod[] = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE'];

function isHttpMethod(value: string): value is HttpMethod {
  return (HTTP_METHODS as readonly string[]).includes(value);
}

function toHttpMethod(method: string | undefined): HttpMethod {
  const upper = (method ?? 'GET').toUpperCase();
  if (isHttpMethod(upper)) return upper;
  throw new TypeError(
    `eliteaFetch: unsupported HTTP method "${method}" — v2.yaml operations use only ${HTTP_METHODS.join('/')}`,
  );
}

/** `RequestInit['headers']` accepts three shapes; `HttpClient` wants a plain record. */
function toHeaderRecord(headers: HeadersInit | undefined): Record<string, string> | undefined {
  if (headers === undefined) return undefined;
  return Object.fromEntries(new Headers(headers).entries());
}

/**
 * orval already JSON-stringifies object bodies before calling the mutator
 * (verified via context7: the generated call site is
 * `customFetch(url, { ...options, method, headers, body: JSON.stringify(data) })`),
 * so the normal case is a string. `null`/`undefined` both mean "no body";
 * anything else is passed through unchanged — `http.ts`'s own
 * `serializeBody` already handles a non-string body defensively.
 */
function toRequestBody(body: BodyInit | null | undefined): unknown {
  if (body === null || body === undefined) return undefined;
  return body;
}

/* ── injected client (R-S2: factory + inject, no module-scope construction) ── */

let activeClient: HttpClient | null = null;

/**
 * Called once by the app layer (R2) after `shared/config`'s `getConfig()`
 * resolves. Returns the client so R2 can also use it directly (e.g. for the
 * auth callback's session probe) without a second construction.
 */
export function configureGeneratedClient(cfg: HttpConfig): HttpClient {
  activeClient = createHttpClient(cfg);
  return activeClient;
}

/** Test-only reset — mirrors the per-test isolation every Wave-1 unit needs. */
export function resetGeneratedClient(): void {
  activeClient = null;
}

function requireClient(): HttpClient {
  if (activeClient === null) {
    throw new Error(
      'eliteaFetch: no HttpClient configured — call configureGeneratedClient(cfg) from app bootstrap (R2) before any generated hook runs',
    );
  }
  return activeClient;
}

/**
 * A generated hook's rejection. `failure` preserves `HttpClient`'s
 * discriminated `HttpFailure` so callers (react-query `onError`, an error
 * boundary) can branch on `error.failure.kind` instead of parsing a message
 * string — `error instanceof EliteaApiError && error.failure.kind === 'auth'`.
 */
export class EliteaApiError extends Error {
  readonly failure: HttpFailure;

  constructor(failure: HttpFailure) {
    super(describeFailure(failure));
    this.name = 'EliteaApiError';
    this.failure = failure;
  }
}

function describeFailure(failure: HttpFailure): string {
  switch (failure.kind) {
    case 'http':
      return `eliteaFetch: ${failure.status} from ${failure.url}`;
    case 'auth':
      return `eliteaFetch: auth failure (${failure.status}) from ${failure.url}`;
    case 'network':
      return `eliteaFetch: network error — ${failure.message}`;
    case 'aborted':
      return `eliteaFetch: aborted request to ${failure.url}`;
    default:
      return failure satisfies never;
  }
}

/**
 * The mutator orval's generated `react-query` + `httpClient: 'fetch'` hooks
 * call. `Promise<T>` never rejects with anything but `EliteaApiError` — a
 * 4xx/5xx/network/abort failure from `HttpClient` is always turned into a
 * thrown `EliteaApiError` (never a bare `Result`), because react-query's
 * `queryFn`/`mutationFn` contract requires a REJECTED promise for the error
 * state, whereas `http.ts` itself returns errors as values (§3.6) — this is
 * the one boundary in the app where a `Result` is deliberately unwrapped
 * back into a throw, and it happens ONLY here.
 *
 * On success, `T` is always the ENVELOPED type orval generates for every
 * operation — `{data, status, headers}` — never the bare body.
 * `override.fetch.includeHttpResponseReturnType` defaults to `true` in
 * orval 8.23.0 (`orval.config.ts` does not override it), and with a custom
 * mutator configured, orval's generated call site is a bare
 * `return eliteaFetch<T>(url, options)` — no wrapping step of its own (see
 * `@orval/fetch/dist/index.mjs`'s `customFetchResponseImplementation`,
 * versus its OWN default-fetch-client body a few lines away, which builds
 * `{data, status, headers}` itself when there is no custom mutator). So the
 * mutator is exactly where that envelope has to be built when a custom
 * mutator is in play — `HttpClient.request`'s `HttpResult` already carries
 * `status`/`data`/the real `headers` (threaded through from the raw
 * `Response` in `http.ts`'s `toResult`) for precisely this.
 */
export async function eliteaFetch<T>(
  url: string,
  options: RequestInit = {},
  /**
   * Transport-level flags that have no `RequestInit` equivalent. `background`
   * marks a peripheral poll whose 401 must not escalate into a re-auth — see
   * `HttpRequestOptions.background` in `shared/api/http.ts` for why that
   * distinction exists. `originRoot` and `binary` are the `/llm` audio
   * routes' needs (`features/chat-input/api/voiceTransport.ts`).
   */
  transport: { readonly background?: boolean; readonly originRoot?: boolean; readonly binary?: boolean } = {},
): Promise<T> {
  const client = requireClient();
  const method = toHttpMethod(options.method);
  const headers = toHeaderRecord(options.headers);
  const body = toRequestBody(options.body);
  // exactOptionalPropertyTypes: HttpRequestOptions declares `signal?:
  // AbortSignal` (never `AbortSignal | undefined`) — an explicit `signal:
  // undefined` key is a type error, so the key is only added when there
  // really is one. Same reasoning for `headers`/`body` below.
  const result = await client.request<unknown>(method, url, {
    ...(options.signal ? { signal: options.signal } : {}),
    ...(headers !== undefined ? { headers } : {}),
    ...(body !== undefined ? { body } : {}),
    ...(transport.background === true ? { background: true } : {}),
    ...(transport.originRoot === true ? { originRoot: true } : {}),
    ...(transport.binary === true ? { binary: true } : {}),
  });
  if (result.ok) {
    return { data: result.data, status: result.status, headers: result.headers } as T;
  }
  throw new EliteaApiError(result.error);
}

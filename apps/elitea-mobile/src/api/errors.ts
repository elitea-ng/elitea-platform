/**
 * Every failure of an API call surfaces as an `ApiError` with a `kind` the UI
 * and retry logic can switch on.
 */

export type SessionInvalidReason =
  | 'device_revoked'
  | 'credential_rejected'
  | 'no_credential'
  | 'stale_credential'
  /** 401 for an expired native access token; a new one is in the session, retry once. */
  | 'refreshed';

export type ApiErrorKind =
  /** The request never got an HTTP answer (offline, DNS, TLS, reset). */
  | 'network'
  /** No answer within the per-call deadline. */
  | 'timeout'
  /** The caller (or endSession) cancelled the request. */
  | 'aborted'
  /** Any other non-2xx answer. */
  | 'http'
  /** 426: this app version is too old for the server. */
  | 'upgrade_required'
  /** 401: the credential is no longer valid (the session has been ended). */
  | 'session_invalid'
  /** The request or a redirect targeted another origin; the token was not sent. */
  | 'origin_mismatch'
  /** `endSession` is running; new requests are refused. */
  | 'session_ending';

export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status?: number;
  /** Machine-readable `error` code from the body, when the server sent one. */
  readonly code?: string;
  readonly reason?: SessionInvalidReason;
  /** Parsed `Retry-After`, in milliseconds. */
  readonly retryAfterMs?: number;

  constructor(
    kind: ApiErrorKind,
    message: string,
    details: {
      status?: number;
      code?: string;
      reason?: SessionInvalidReason;
      retryAfterMs?: number;
      cause?: unknown;
    } = {},
  ) {
    super(message, details.cause === undefined ? undefined : { cause: details.cause });
    this.name = 'ApiError';
    this.kind = kind;
    this.status = details.status;
    this.code = details.code;
    this.reason = details.reason;
    this.retryAfterMs = details.retryAfterMs;
  }
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError;
}

/** `Retry-After` as delay-seconds or an HTTP date, in milliseconds; undefined when absent or invalid. */
export function parseRetryAfter(value: string | null, now: number = Date.now()): number | undefined {
  if (value === null) return undefined;
  const trimmed = value.trim();
  if (/^\d+$/.test(trimmed)) return Number(trimmed) * 1000;
  const date = Date.parse(trimmed);
  if (Number.isNaN(date)) return undefined;
  return Math.max(0, date - now);
}

function errorCodeOf(body: unknown): string | undefined {
  if (body !== null && typeof body === 'object' && 'error' in body) {
    const code = (body as { error: unknown }).error;
    if (typeof code === 'string' && code.length <= 200) return code;
  }
  return undefined;
}

/**
 * Turns an openapi-fetch result into its data, or throws `ApiError('http')`
 * for a non-2xx answer. (Transport, auth and version failures already throw
 * from the guarded fetch before reaching here.)
 */
export async function unwrap<T>(
  call: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  const { data, error, response } = await call;
  if (response.ok) return data as T;
  throw new ApiError('http', `HTTP ${response.status}`, {
    status: response.status,
    code: errorCodeOf(error),
    retryAfterMs: parseRetryAfter(response.headers.get('Retry-After')),
  });
}

/**
 * URL helpers for the token-leak guard.
 *
 * The bearer token may only ever be sent to the connected workspace origin.
 * Server-supplied URLs (an execution's `events_url`, later brand-pack assets)
 * are validated here before anything is fetched from them.
 *
 * Implemented without the global `URL` class, which is incomplete in React
 * Native's polyfill and must behave identically in jest and on device.
 */

/** Every REST operation in the contract is served under this prefix. */
export const API_PREFIX = '/api/v2';

/** Longest single path segment accepted from the server (execution ids are far shorter). */
export const MAX_SEGMENT_BYTES = 512;
/** Longest whole path accepted from the server. */
export const MAX_PATH_BYTES = 2048;

const ORIGIN_RE = /^(https?):\/\/([^/?#\\@\s]+)(?=$|[/?#])/i;
const SEGMENT_RE = /^[A-Za-z0-9._~-]+$/;

function utf8Length(value: string): number {
  let bytes = 0;
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0;
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
  }
  return bytes;
}

/**
 * The normalised origin (`scheme://host[:port]`, lower-case, default port
 * dropped) of an absolute http(s) URL, or null for anything else.
 */
export function originOf(url: string): string | null {
  const match = ORIGIN_RE.exec(url);
  if (!match) return null;
  const scheme = match[1].toLowerCase();
  let authority = match[2].toLowerCase();
  const defaultPort = scheme === 'https' ? ':443' : ':80';
  if (authority.endsWith(defaultPort)) authority = authority.slice(0, -defaultPort.length);
  return `${scheme}://${authority}`;
}

/** True when `url` is an absolute URL on exactly `origin`. */
export function isSameOrigin(url: string, origin: string): boolean {
  const actual = originOf(url);
  return actual !== null && actual === originOf(origin);
}

/**
 * True when `path` is a plain, absolute, same-origin path under `/api/v2/`:
 * no scheme or authority (`https://…`, `//host`), no dot segments, no
 * backslashes, no query or fragment, no control characters and no
 * percent-encoding, with every segment at most 512 bytes.
 */
export function isSameOriginApiPath(path: string): boolean {
  if (typeof path !== 'string') return false;
  if (!path.startsWith(`${API_PREFIX}/`)) return false;
  if (utf8Length(path) > MAX_PATH_BYTES) return false;
  const segments = path.slice(1).split('/');
  for (const segment of segments) {
    if (segment === '' || segment === '.' || segment === '..') return false;
    if (!SEGMENT_RE.test(segment)) return false;
    if (utf8Length(segment) > MAX_SEGMENT_BYTES) return false;
  }
  return true;
}

/**
 * Resolves a server-supplied API path against the workspace origin, or returns
 * null when the path fails `isSameOriginApiPath`.
 */
export function resolveApiPath(origin: string, path: string): string | null {
  const base = originOf(origin);
  if (base === null || !isSameOriginApiPath(path)) return null;
  return `${base}${path}`;
}

/** Base URL for the generated REST client. */
export function apiBaseUrl(origin: string): string {
  const base = originOf(origin);
  if (base === null) throw new Error('workspace origin is not an absolute http(s) URL');
  return `${base}${API_PREFIX}`;
}

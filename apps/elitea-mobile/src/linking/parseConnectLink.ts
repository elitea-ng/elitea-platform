/**
 * Parses everything a user can hand us to identify a workspace server and
 * returns a normalized origin (`https://host[:port]`).
 *
 * Accepted inputs:
 *  - connect links: `<scheme>://connect?server=<url-encoded https URL>`, where
 *    `<scheme>` is one of the configured connect schemes (`zefir`, `agentzefir`)
 *  - a bare URL such as a QR payload or a typed address: `https://host[:port][/any/path]`
 *  - a typed host without a scheme (`host[:port]`), which is treated as https
 *
 * Paths, queries and fragments are stripped. Credentials in the URL are rejected.
 *
 * Deliberately implemented without the global `URL` class: React Native's URL
 * polyfill is incomplete and this function must behave identically in Jest and
 * on device.
 */

export type ConnectLinkErrorCode =
  | 'empty'
  | 'unsupported_scheme'
  | 'not_connect_link'
  | 'missing_server'
  | 'ambiguous_server'
  | 'nested_link'
  | 'insecure_http'
  | 'credentials_in_url'
  | 'invalid_host'
  | 'invalid_port'
  | 'malformed';

export type ConnectLinkResult =
  | { ok: true; origin: string }
  | { ok: false; error: ConnectLinkErrorCode };

export interface ParseConnectLinkOptions {
  /** Schemes accepted for `<scheme>://connect?server=...` links. Pass `identity.connectSchemes`. */
  connectSchemes: readonly string[];
  /**
   * Allow plain `http://` for loopback and private LAN hosts (localhost, 127.x,
   * 10.x, 192.168.x). FLAG: intended for development against a local server only;
   * callers should pass `__DEV__` (or a server-driven policy) — never `true`
   * unconditionally in a release build.
   */
  allowPrivateHttp?: boolean;
}

const SCHEME_RE = /^([a-z][a-z0-9+.-]*):\/\/(.*)$/i;
const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const IPV4_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const IPV6_RE = /^\[[0-9a-f:.]+\]$/;
const FORBIDDEN_CHARS_RE = /[\s\u0000-\u001f\u007f\\]/;

const fail = (error: ConnectLinkErrorCode): ConnectLinkResult => ({ ok: false, error });

export function parseConnectLink(
  input: string,
  options: ParseConnectLinkOptions,
): ConnectLinkResult {
  const raw = input.trim();
  if (raw === '') return fail('empty');
  if (FORBIDDEN_CHARS_RE.test(raw)) return fail('malformed');

  const match = SCHEME_RE.exec(raw);
  if (!match) {
    // No "scheme://": a typed host such as `ai.example.com` or `ai.example.com:8443/x`.
    return parseHttpUrl('https', raw, options);
  }

  const scheme = match[1].toLowerCase();
  const rest = match[2];
  const connectSchemes = options.connectSchemes.map((s) => s.toLowerCase());

  if (connectSchemes.includes(scheme)) {
    return parseConnectPayload(rest, options);
  }
  if (scheme === 'https' || scheme === 'http') {
    return parseHttpUrl(scheme, rest, options);
  }
  return fail('unsupported_scheme');
}

function parseConnectPayload(rest: string, options: ParseConnectLinkOptions): ConnectLinkResult {
  const withoutFragment = rest.split('#', 1)[0];
  const queryStart = withoutFragment.indexOf('?');
  const target = queryStart === -1 ? withoutFragment : withoutFragment.slice(0, queryStart);
  const query = queryStart === -1 ? '' : withoutFragment.slice(queryStart + 1);

  // Accept `connect` and `connect/` as the host-part action.
  if (target.replace(/\/+$/, '').toLowerCase() !== 'connect') return fail('not_connect_link');

  const servers: string[] = [];
  for (const pair of query.split('&')) {
    if (pair === '') continue;
    const eq = pair.indexOf('=');
    const key = eq === -1 ? pair : pair.slice(0, eq);
    if (key !== 'server') continue;
    const value = eq === -1 ? '' : pair.slice(eq + 1);
    try {
      servers.push(decodeURIComponent(value.replace(/\+/g, ' ')).trim());
    } catch {
      return fail('malformed');
    }
  }
  if (servers.length > 1) return fail('ambiguous_server');
  const server = servers[0];
  if (!server) return fail('missing_server');
  if (FORBIDDEN_CHARS_RE.test(server)) return fail('malformed');

  // The embedded server must itself be an http(s) URL — never another connect link.
  const inner = SCHEME_RE.exec(server);
  if (!inner) return parseHttpUrl('https', server, options);
  const innerScheme = inner[1].toLowerCase();
  if (innerScheme === 'https' || innerScheme === 'http') {
    return parseHttpUrl(innerScheme, inner[2], options);
  }
  return fail('nested_link');
}

function parseHttpUrl(
  scheme: 'http' | 'https',
  rest: string,
  options: ParseConnectLinkOptions,
): ConnectLinkResult {
  // Authority ends at the first '/', '?' or '#'; everything after is stripped.
  const authority = rest.split(/[/?#]/, 1)[0];
  if (authority === '') return fail('invalid_host');
  if (authority.includes('@')) return fail('credentials_in_url');

  let host: string;
  let portText: string | undefined;
  if (authority.startsWith('[')) {
    const close = authority.indexOf(']');
    if (close === -1) return fail('invalid_host');
    host = authority.slice(0, close + 1).toLowerCase();
    const after = authority.slice(close + 1);
    if (after !== '') {
      if (!after.startsWith(':')) return fail('invalid_host');
      portText = after.slice(1);
    }
    if (!IPV6_RE.test(host)) return fail('invalid_host');
  } else {
    const parts = authority.split(':');
    if (parts.length > 2) return fail('invalid_host');
    host = parts[0].toLowerCase().replace(/\.$/, '');
    portText = parts[1];
    if (!isValidHostname(host)) return fail('invalid_host');
  }

  let port = '';
  if (portText !== undefined) {
    if (!/^\d{1,5}$/.test(portText)) return fail('invalid_port');
    const n = Number(portText);
    if (n < 1 || n > 65535) return fail('invalid_port');
    const isDefault = (scheme === 'https' && n === 443) || (scheme === 'http' && n === 80);
    port = isDefault ? '' : `:${n}`;
  }

  if (scheme === 'http' && !(options.allowPrivateHttp && isPrivateHost(host))) {
    return fail('insecure_http');
  }

  return { ok: true, origin: `${scheme}://${host}${port}` };
}

function isValidHostname(host: string): boolean {
  if (host.length === 0 || host.length > 253) return false;
  const ipv4 = IPV4_RE.exec(host);
  if (ipv4) return ipv4.slice(1).every((octet) => Number(octet) <= 255);
  // TODO: internationalized domain names must currently be entered in punycode.
  return host.split('.').every((label) => LABEL_RE.test(label));
}

/** Loopback and RFC 1918 ranges the product explicitly allows plain HTTP for (dev flag). */
function isPrivateHost(host: string): boolean {
  if (host === 'localhost' || host === '[::1]') return true;
  const ipv4 = IPV4_RE.exec(host);
  if (!ipv4) return false;
  const [a, b] = [Number(ipv4[1]), Number(ipv4[2])];
  return a === 127 || a === 10 || (a === 192 && b === 168);
}

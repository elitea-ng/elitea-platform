/**
 * PKCE / OAuth security-parameter helpers — faithful port of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpCrypto.helpers.js
 * (unit A5, spec §9.3). Pure functions over Web Crypto; no network, no
 * storage.
 */

export function base64UrlEncode(buffer: ArrayBuffer | Uint8Array): string {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export function randomString(length = 32): string {
  const arr = new Uint8Array(length);
  crypto.getRandomValues(arr);
  return base64UrlEncode(arr);
}

export function generateSessionId(): string {
  return crypto.randomUUID();
}

/** SHA-256 PKCE code challenge. Base64url-encoded digest is always exactly 43 characters. */
export async function sha256(message: string): Promise<string> {
  const encoder = new TextEncoder();
  const data = encoder.encode(message);
  const hashBuffer = await crypto.subtle.digest('SHA-256', data);
  const encoded = base64UrlEncode(hashBuffer);

  if (encoded.length !== 43) {
    throw new Error(`code_challenge must be 43 characters, got ${encoded.length}`);
  }

  return encoded;
}

/** Normalises a raw scope string, ensuring `openid` leads for OIDC flows. OAuth separates scopes with spaces (RFC 6749 3.3), but toolkit credentials ship them as "a, b, c"; a comma left on a token (`offline_access,`) is an unknown scope to Entra (AADSTS650053), so commas split too. */
export function normalizeScope(scope: string | undefined | null, isOIDC: boolean): string {
  if (!scope) return isOIDC ? 'openid' : '';
  const scopes = scope.split(/[\s,]+/).filter(Boolean);
  if (isOIDC && !scopes.includes('openid')) scopes.unshift('openid');
  return scopes.join(' ');
}

/**
 * OIDC detection for user-login flows: requires BOTH an issuer/userinfo
 * endpoint AND advertised `openid` scope support. GitHub-style servers can
 * carry `issuer` (for Actions OIDC, machine-to-machine) without supporting
 * user OIDC login — the `openid` scope check is what excludes them.
 */
export function isOIDCFlow(metadata: { issuer?: string | undefined; userinfo_endpoint?: string | undefined; scopes_supported?: readonly string[] | undefined } | undefined | null): boolean {
  const hasIssuer = Boolean(metadata?.issuer || metadata?.userinfo_endpoint);
  const supportsOpenid = metadata?.scopes_supported?.includes('openid') ?? false;
  return hasIssuer && supportsOpenid;
}

import { z } from 'zod';

import { anonymousFetch, readJson, type AnonymousFetchOptions } from '@/api/anonymous';
import { isApiError, parseRetryAfter } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { log } from '@/lib/log';

/**
 * The native token and revocation endpoints (ADR-0025 WP2/WP3), spoken by
 * hand rather than through a generic OAuth helper because the token response
 * also carries the client policy and the 401 body distinguishes a revoked
 * device (`{"error":"device_revoked"}`, wipe) from an unknown client.
 *
 * Every request is anonymous (no Authorization header: the endpoints refuse
 * one), form-encoded, origin-guarded and carries X-Client-Version. Redirects
 * are refused outright (SEC-2, src/api/noRedirectFetch.ts): a followed 307/308
 * would re-send the code, verifier or refresh token to the new location.
 */

const OPAQUE_TOKEN = z.string().min(16).max(1024);

export const TokenResponseSchema = z.looseObject({
  access_token: OPAQUE_TOKEN,
  token_type: z.string().regex(/^bearer$/i),
  // 0 is a real answer: a re-delivered pair (grace window) reports what is
  // left of its access token, truncated to seconds. The pair still rotates;
  // the access token is simply refreshed before first use.
  expires_in: z.number().int().nonnegative().max(366 * 24 * 3600),
  refresh_token: OPAQUE_TOKEN,
  refresh_token_expires_in: z.number().int().nonnegative().max(10 * 366 * 24 * 3600),
  device_id: z.string().min(1).max(128),
  // Validated field by field by the policy store; absent when the server
  // could not read its policy (the app keeps the last one).
  client_policy: z.record(z.string(), z.unknown()).optional(),
});

export type TokenResponse = z.infer<typeof TokenResponseSchema>;

type Assignable<A, B> = A extends B ? true : false;
type TokenFields =
  | 'access_token'
  | 'token_type'
  | 'expires_in'
  | 'refresh_token'
  | 'refresh_token_expires_in'
  | 'device_id';
const contractCheck: Assignable<
  Pick<TokenResponse, TokenFields>,
  Pick<components['schemas']['NativeTokenResponse'], TokenFields>
> = true;
void contractCheck;

export type TokenOutcome =
  | { kind: 'ok'; response: TokenResponse; receivedAt: number }
  /** The device session is revoked, expired, deactivated or a token was reused: wipe. */
  | { kind: 'device_revoked' }
  /** The code or refresh token is invalid, expired or already used. */
  | { kind: 'invalid_grant' }
  /** The app is not (or no longer) a registered, enabled client of this workspace. */
  | { kind: 'invalid_client' }
  /** 426: the app is older than the workspace minimum (the session shows the update screen). */
  | { kind: 'upgrade_required' }
  /** 429/503/5xx: try again later; nothing was consumed. */
  | { kind: 'unavailable'; retryAfterMs?: number }
  /** No HTTP answer: the request may or may not have reached the server. */
  | { kind: 'network' }
  /** Any other refusal (malformed request) or an unreadable 200. */
  | { kind: 'failed'; error?: string };

const OAuthErrorSchema = z.looseObject({ error: z.string().max(100) });

function formBody(params: Record<string, string>): string {
  return Object.entries(params)
    .map(([key, value]) => `${encodeURIComponent(key)}=${encodeURIComponent(value)}`)
    .join('&');
}

const FORM_HEADERS = { 'Content-Type': 'application/x-www-form-urlencoded' };

async function postTokenRequest(
  origin: string,
  endpoint: string,
  params: Record<string, string>,
  options: AnonymousFetchOptions & { now?: () => number } = {},
): Promise<TokenOutcome> {
  const now = options.now ?? Date.now;
  let response: Response;
  try {
    response = await anonymousFetch(
      origin,
      endpoint,
      { method: 'POST', headers: FORM_HEADERS, body: formBody(params) },
      { ...options, fetch: options.fetch ?? noRedirectFetch },
    );
  } catch (error) {
    if (isApiError(error)) {
      if (error.kind === 'upgrade_required') return { kind: 'upgrade_required' };
      if (error.kind === 'origin_mismatch') return { kind: 'failed', error: 'origin_mismatch' };
    }
    return { kind: 'network' };
  }
  const body = await readJson(response);
  if (response.status !== 200) {
    const code = OAuthErrorSchema.safeParse(body);
    const error = code.success ? code.data.error : undefined;
    if (response.status === 401 && error === 'device_revoked') return { kind: 'device_revoked' };
    // Only the token handler's own refusals speak about this client: 401
    // invalid_client, and 404 {"error":"not_found"} while no native client is
    // registered at all. A bare 401/404 from something in front of it (an
    // edge route not deployed yet, a proxy) must not end the session.
    if (response.status === 401 && error === 'invalid_client') return { kind: 'invalid_client' };
    if (response.status === 404 && error === 'not_found') return { kind: 'invalid_client' };
    if (response.status === 400 && error === 'invalid_grant') return { kind: 'invalid_grant' };
    if (response.status === 429 || response.status === 503 || response.status >= 500) {
      return { kind: 'unavailable', retryAfterMs: parseRetryAfter(response.headers.get('Retry-After')) };
    }
    return { kind: 'failed', error };
  }
  const parsed = TokenResponseSchema.safeParse(body);
  if (!parsed.success) {
    log.warn('token response rejected', { issues: parsed.error.issues.length });
    return { kind: 'failed', error: 'invalid_response' };
  }
  return { kind: 'ok', response: parsed.data, receivedAt: now() };
}

export interface CodeExchange {
  code: string;
  codeVerifier: string;
  redirectUri: string;
  clientId: string;
}

/** `grant_type=authorization_code` (RFC 6749 §4.1.3 with the PKCE verifier). */
export function exchangeAuthorizationCode(
  origin: string,
  tokenEndpoint: string,
  request: CodeExchange,
  options: AnonymousFetchOptions & { now?: () => number } = {},
): Promise<TokenOutcome> {
  return postTokenRequest(
    origin,
    tokenEndpoint,
    {
      grant_type: 'authorization_code',
      code: request.code,
      redirect_uri: request.redirectUri,
      client_id: request.clientId,
      code_verifier: request.codeVerifier,
    },
    options,
  );
}

/** `grant_type=refresh_token`; the server rotates the refresh token on every success. */
export function refreshTokens(
  origin: string,
  tokenEndpoint: string,
  request: { refreshToken: string; clientId: string; clientVersion: string },
  options: AnonymousFetchOptions & { now?: () => number } = {},
): Promise<TokenOutcome> {
  return postTokenRequest(
    origin,
    tokenEndpoint,
    {
      grant_type: 'refresh_token',
      refresh_token: request.refreshToken,
      client_id: request.clientId,
      client_version: request.clientVersion,
    },
    options,
  );
}

/** RFC 7009 revoke of the whole device session. True on 200 (also for an unknown token). */
export async function revokeToken(
  origin: string,
  revocationEndpoint: string,
  request: { token: string; clientId: string },
  options: AnonymousFetchOptions & { signal?: AbortSignal } = {},
): Promise<boolean> {
  try {
    const response = await anonymousFetch(
      origin,
      revocationEndpoint,
      {
        method: 'POST',
        headers: FORM_HEADERS,
        body: formBody({ token: request.token, token_type_hint: 'refresh_token', client_id: request.clientId }),
        signal: options.signal,
      },
      { ...options, fetch: options.fetch ?? noRedirectFetch },
    );
    return response.status === 200;
  } catch (error) {
    log.info('native revoke failed', { error });
    return false;
  }
}

export const __formBodyForTests = formBody;

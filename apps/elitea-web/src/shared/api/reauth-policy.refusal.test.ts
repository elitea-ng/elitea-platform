/**
 * The 401 vocabulary this app reads back from elitea-main (#538).
 *
 * The bodies below are the exact pairs
 * `services/elitea-main/internal/api/credential_refusal_code_route_test.go`
 * pins on the server, so the two sides of the contract fail together rather
 * than drifting apart in silence.
 */
import { describe, expect, it } from 'vitest';

import { credentialRefusalCode } from './reauth-policy';

function refusal(code: string, message: string, status = 401): Response {
  return new Response(JSON.stringify({ error: { message, type: 'authentication_error', code } }), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

describe('credentialRefusalCode — which refusal a 401 is', () => {
  it.each([
    ['no_credential', 'missing authorization header'],
    ['session_cookie_signature_mismatch', 'the session cookie signature does not match'],
    ['session_cookie_expired', 'the session cookie expired'],
    ['session_cookie_malformed', 'the session cookie is malformed'],
    ['session_revoked', 'the session was revoked'],
    ['session_expired', 'the session expired'],
    ['session_idle', 'the session was idle for too long'],
    ['session_unknown', 'the session is not known to this deployment'],
    ['token_rejected', 'token validation failed'],
    ['authorization_scheme_unsupported', 'unsupported authorization scheme'],
  ])('reads %s', async (code, message) => {
    expect(await credentialRefusalCode(refusal(code, message))).toBe(code);
  });

  it('reads the generic refusal the server keeps for its own configuration', async () => {
    // `session_secret_not_configured` is deliberately NOT disclosed: the
    // server answers `unauthenticated` and logs the real reason.
    expect(await credentialRefusalCode(refusal('unauthenticated', 'missing authorization header'))).toBe(
      'unauthenticated',
    );
  });

  it('names nothing for a 401 whose body is not the error envelope', async () => {
    const flat = new Response(JSON.stringify({ error: 'missing authorization header' }), {
      status: 401,
      headers: { 'content-type': 'application/json' },
    });
    expect(await credentialRefusalCode(flat)).toBeUndefined();
  });

  it('names nothing for a 401 that is not JSON, and does not throw', async () => {
    const text = new Response('nope', { status: 401, headers: { 'content-type': 'text/plain' } });
    expect(await credentialRefusalCode(text)).toBeUndefined();
  });

  it('names nothing for a 401 that lies about being JSON', async () => {
    const liar = new Response('nope', { status: 401, headers: { 'content-type': 'application/json' } });
    expect(await credentialRefusalCode(liar)).toBeUndefined();
  });

  it('ignores every status but 401 — a 403 is a resource refusal, not a session one', async () => {
    expect(await credentialRefusalCode(refusal('session_revoked', 'the session was revoked', 403))).toBeUndefined();
  });

  it('leaves the body consumable, because it reads a clone', async () => {
    const response = refusal('session_revoked', 'the session was revoked');
    await credentialRefusalCode(response);
    expect(((await response.json()) as { error: { code: string } }).error.code).toBe('session_revoked');
  });
});

import { describe, expect, it } from 'vitest';

import { base64UrlEncode, generateSessionId, isOIDCFlow, normalizeScope, randomString, sha256 } from './crypto';

describe('base64UrlEncode', () => {
  it('encodes bytes as base64url with no padding and swapped chars', () => {
    // 0xFB 0xFF encodes to base64 "+/8" + "=" padding; base64url must swap and strip.
    const bytes = new Uint8Array([0xfb, 0xff]);
    const encoded = base64UrlEncode(bytes);
    expect(encoded).not.toMatch(/[+/=]/);
    expect(encoded).toBe('-_8');
  });

  it('accepts a raw ArrayBuffer as well as a Uint8Array', () => {
    const buf = new Uint8Array([1, 2, 3]).buffer;
    expect(base64UrlEncode(buf)).toBe(base64UrlEncode(new Uint8Array(buf)));
  });
});

describe('randomString', () => {
  it('defaults to 32 raw bytes -> base64url length in [39,43] with no padding chars', () => {
    const s = randomString();
    expect(s).not.toMatch(/[+/=]/);
    // 32 bytes -> ceil(32*4/3) = 43 base64 chars minus up to 1 padding char removed.
    expect(s.length).toBeGreaterThanOrEqual(42);
    expect(s.length).toBeLessThanOrEqual(43);
  });

  it('respects an explicit length and is not trivially repeatable', () => {
    const a = randomString(8);
    const b = randomString(8);
    expect(a).not.toBe(b);
  });
});

describe('generateSessionId', () => {
  it('returns a real UUID (crypto.randomUUID, not the old Math.random shim)', () => {
    const id = generateSessionId();
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i);
  });
});

describe('sha256', () => {
  it('produces a 43-character base64url PKCE code_challenge', async () => {
    const challenge = await sha256('some-code-verifier');
    expect(challenge).toHaveLength(43);
    expect(challenge).not.toMatch(/[+/=]/);
  });

  it('is deterministic for the same input', async () => {
    const a = await sha256('fixed-verifier');
    const b = await sha256('fixed-verifier');
    expect(a).toBe(b);
  });

  it('differs for different inputs', async () => {
    const a = await sha256('verifier-a');
    const b = await sha256('verifier-b');
    expect(a).not.toBe(b);
  });
});

describe('normalizeScope', () => {
  it('splits comma-separated scopes (#2138)', () => {
    expect(normalizeScope('offline_access, Files.ReadWrite.All,Sites.Selected', false)).toBe('offline_access Files.ReadWrite.All Sites.Selected');
  });

  it('returns empty string when no scope and not OIDC', () => {
    expect(normalizeScope(undefined, false)).toBe('');
    expect(normalizeScope('', false)).toBe('');
  });

  it('returns "openid" when no scope and IS OIDC', () => {
    expect(normalizeScope(undefined, true)).toBe('openid');
  });

  it('prepends openid for OIDC flows missing it', () => {
    expect(normalizeScope('read write', true)).toBe('openid read write');
  });

  it('does not duplicate openid when already present', () => {
    expect(normalizeScope('openid read', true)).toBe('openid read');
  });

  it('filters extra whitespace between scopes', () => {
    expect(normalizeScope('read   write', false)).toBe('read write');
  });

  it('leaves non-OIDC scopes untouched', () => {
    expect(normalizeScope('repo user', false)).toBe('repo user');
  });
});

describe('isOIDCFlow', () => {
  it('false when metadata is absent', () => {
    expect(isOIDCFlow(undefined)).toBe(false);
    expect(isOIDCFlow(null)).toBe(false);
  });

  it('false when issuer present but openid scope not advertised (GitHub Actions OIDC case)', () => {
    expect(isOIDCFlow({ issuer: 'https://token.actions.githubusercontent.com', scopes_supported: ['repo'] })).toBe(
      false,
    );
  });

  it('false when openid scope advertised but no issuer/userinfo endpoint', () => {
    expect(isOIDCFlow({ scopes_supported: ['openid'] })).toBe(false);
  });

  it('true when both issuer and openid scope are present', () => {
    expect(isOIDCFlow({ issuer: 'https://accounts.example.com', scopes_supported: ['openid', 'profile'] })).toBe(
      true,
    );
  });

  it('true via userinfo_endpoint instead of issuer', () => {
    expect(
      isOIDCFlow({ userinfo_endpoint: 'https://example.com/userinfo', scopes_supported: ['openid'] }),
    ).toBe(true);
  });
});

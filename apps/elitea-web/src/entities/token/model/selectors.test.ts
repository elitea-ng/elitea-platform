import { describe, expect, it } from 'vitest';

import {
  bindableProjectId,
  maskedTokenValue,
  sortTokensByName,
  tokenExpiryInDays,
  tokenExpiryStatus,
  tokenMintRefusal,
  tokenProjectErrorCode,
  tokenProjectKey,
} from './selectors';
import type { PersonalAccessToken } from './types';

const NOW = new Date('2026-01-01T00:00:00Z').getTime();
const DAY = 24 * 3600 * 1000;

const token = (name: string, expires: string | null): PersonalAccessToken => ({
  uuid: name,
  name,
  token: `sk-${name}-abcd1234`,
  expires,
});

describe('tokenExpiryInDays', () => {
  it('returns -1 for a null (never-expiring) token', () => {
    expect(tokenExpiryInDays(null, NOW)).toBe(-1);
  });

  it('rounds whole days when more than a day remains', () => {
    const expires = new Date(NOW + 10 * DAY).toISOString();
    expect(tokenExpiryInDays(expires, NOW)).toBe(10);
  });

  it('returns 1 when less than a day but still positive remains', () => {
    const expires = new Date(NOW + DAY / 2).toISOString();
    expect(tokenExpiryInDays(expires, NOW)).toBe(1);
  });

  it('returns 0 for an already-expired token', () => {
    const expires = new Date(NOW - DAY).toISOString();
    expect(tokenExpiryInDays(expires, NOW)).toBe(0);
  });

  it('returns 0 at exactly the expiry instant', () => {
    const expires = new Date(NOW).toISOString();
    expect(tokenExpiryInDays(expires, NOW)).toBe(0);
  });
});

describe('tokenExpiryStatus', () => {
  it('is safe for more than 7 days remaining', () => {
    expect(tokenExpiryStatus(new Date(NOW + 10 * DAY).toISOString(), NOW)).toBe('safe');
  });

  it('is warning at the 7-day boundary and below', () => {
    expect(tokenExpiryStatus(new Date(NOW + 7 * DAY).toISOString(), NOW)).toBe('warning');
    expect(tokenExpiryStatus(new Date(NOW + DAY / 2).toISOString(), NOW)).toBe('warning');
  });

  it('is never for a null expiry', () => {
    expect(tokenExpiryStatus(null, NOW)).toBe('never');
  });

  it('is expired for a past date', () => {
    expect(tokenExpiryStatus(new Date(NOW - DAY).toISOString(), NOW)).toBe('expired');
  });
});

describe('maskedTokenValue', () => {
  it('shows only the last 4 characters, prefixed with an ellipsis', () => {
    expect(maskedTokenValue(token('a', null))).toBe('...1234');
  });
});

describe('sortTokensByName', () => {
  it('sorts case-insensitively', () => {
    const tokens = [token('zeta', null), token('Alpha', null)];
    expect(sortTokensByName(tokens).map((t) => t.name)).toEqual(['Alpha', 'zeta']);
  });
});

describe('tokenProjectKey', () => {
  it('returns the bound project as a string key', () => {
    expect(tokenProjectKey({ ...token('a', null), project_id: 42 })).toBe('42');
  });

  it('treats an explicit null binding as unbound', () => {
    expect(tokenProjectKey({ ...token('a', null), project_id: null })).toBeNull();
  });

  it('treats a record with no project_id field at all as unbound', () => {
    expect(tokenProjectKey(token('a', null))).toBeNull();
  });
});

describe('bindableProjectId', () => {
  it('converts a positive integer id to a number', () => {
    expect(bindableProjectId('42')).toBe(42);
  });

  /*
   * Every case below must give `undefined`, because the caller turns
   * `undefined` into an ABSENT `project_id` — the unbound default (§4). A
   * `0` or a `NaN` here would reach the wire as a real, wrong binding.
   */
  it('gives undefined for an empty selection', () => {
    expect(bindableProjectId('')).toBeUndefined();
  });

  it('gives undefined for zero', () => {
    expect(bindableProjectId('0')).toBeUndefined();
  });

  it('gives undefined for an id that is not a plain positive integer', () => {
    expect(bindableProjectId('-1')).toBeUndefined();
    expect(bindableProjectId('4.2')).toBeUndefined();
    expect(bindableProjectId(' 42')).toBeUndefined();
    expect(bindableProjectId('42x')).toBeUndefined();
    expect(bindableProjectId('personal')).toBeUndefined();
  });

  it('gives undefined for an id above the safe-integer range', () => {
    expect(bindableProjectId('9007199254740993')).toBeUndefined();
  });
});

describe('tokenProjectErrorCode', () => {
  /** The nested envelope the two §4 project failures use. */
  const nested = (code: string): unknown => ({
    failure: { kind: 'http', status: 403, url: '/api/v2/auth/token/', body: { error: { message: 'no', type: 'permission_error', code } } },
  });

  it('reads project_forbidden out of the nested error envelope', () => {
    expect(tokenProjectErrorCode(nested('project_forbidden'))).toBe('project_forbidden');
  });

  it('reads invalid_project_id out of the nested error envelope', () => {
    expect(tokenProjectErrorCode(nested('invalid_project_id'))).toBe('invalid_project_id');
  });

  it('ignores a code it does not know', () => {
    expect(tokenProjectErrorCode(nested('some_other_code'))).toBeNull();
  });

  /*
   * The other half of the split contract: every OTHER failure on this
   * endpoint keeps the flat `{"error":"…"}` shape, where `error` is a string
   * and not an object. It must degrade to null, not throw.
   */
  it('returns null for the flat error envelope', () => {
    expect(
      tokenProjectErrorCode({ failure: { kind: 'http', status: 500, url: '/x', body: { error: 'boom' } } }),
    ).toBeNull();
  });

  it('returns null for a rejection that carries no failure at all', () => {
    expect(tokenProjectErrorCode(new Error('network down'))).toBeNull();
    expect(tokenProjectErrorCode(undefined)).toBeNull();
    expect(tokenProjectErrorCode(null)).toBeNull();
    expect(tokenProjectErrorCode('nope')).toBeNull();
  });
});

describe('tokenMintRefusal', () => {
  const nested = (code: string, extra: Record<string, unknown> = {}): unknown => ({
    failure: {
      kind: 'http',
      status: 403,
      url: '/api/v2/auth/token/',
      body: { error: { message: 'no', type: 'permission_error', code, ...extra } },
    },
  });

  it.each([
    'token_mint_browser_session_required',
    'token_mint_native_admin',
    'token_mint_wider_than_caller',
    'token_mint_caller_unresolved',
  ])('reads %s out of the nested error envelope', (code) => {
    expect(tokenMintRefusal(nested(code))).toEqual({ code, projectId: null });
  });

  it('reads the caller binding a wider-than-caller refusal carries', () => {
    expect(tokenMintRefusal(nested('token_mint_wider_than_caller', { project_id: 5 }))).toEqual({
      code: 'token_mint_wider_than_caller',
      projectId: 5,
    });
    expect(tokenMintRefusal(nested('token_mint_wider_than_caller', { project_id: '5' }))?.projectId).toBeNull();
  });

  it('ignores the project codes, unknown codes and the flat envelope', () => {
    expect(tokenMintRefusal(nested('project_forbidden'))).toBeNull();
    expect(tokenMintRefusal(nested('some_other_code'))).toBeNull();
    expect(tokenMintRefusal({ failure: { kind: 'http', status: 403, url: '/x', body: { error: 'boom' } } })).toBeNull();
    expect(tokenMintRefusal(undefined)).toBeNull();
  });
});

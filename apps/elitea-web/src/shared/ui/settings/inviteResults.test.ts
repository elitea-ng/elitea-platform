/**
 * The bulk-invite reader, tested at the two places it decides something:
 * turning a paste into addresses, and turning the server's array into rows an
 * operator can read.
 */
import { describe, expect, it } from 'vitest';

import {
  hasInviteFailures,
  invalidInviteAddresses,
  isValidInviteAddress,
  parseInviteAddresses,
  readInviteRows,
  readInviteRowsFromError,
  summariseInviteRows,
} from './inviteResults';

describe('parseInviteAddresses', () => {
  it('splits on comma, the reference separator', () => {
    expect(parseInviteAddresses('a@x.io, b@x.io')).toEqual(['a@x.io', 'b@x.io']);
  });

  it('splits on newline and semicolon, which is what a paste actually contains', () => {
    expect(parseInviteAddresses('a@x.io\nb@x.io;c@x.io')).toEqual(['a@x.io', 'b@x.io', 'c@x.io']);
  });

  it('drops empty fragments from trailing separators', () => {
    expect(parseInviteAddresses('a@x.io,,  ,\n')).toEqual(['a@x.io']);
  });

  it('keeps the typed order and removes case-insensitive duplicates', () => {
    // A repeated address would otherwise report `already_member` against a
    // membership the FIRST copy of the same submit had just created.
    expect(parseInviteAddresses('b@x.io, A@x.io, a@X.io')).toEqual(['b@x.io', 'A@x.io']);
  });

  it('is empty for an empty field', () => {
    expect(parseInviteAddresses('   ')).toEqual([]);
  });
});

describe('isValidInviteAddress', () => {
  it('accepts an ordinary address', () => {
    expect(isValidInviteAddress('someone@example.com')).toBe(true);
  });

  it('refuses an address with no domain dot', () => {
    expect(isValidInviteAddress('someone@localhost')).toBe(false);
  });

  it('refuses a bare word', () => {
    expect(isValidInviteAddress('not-an-email')).toBe(false);
  });

  it('names only the invalid ones', () => {
    expect(invalidInviteAddresses(['ok@x.io', 'nope', 'also@y.io'])).toEqual(['nope']);
  });
});

describe('readInviteRows', () => {
  it('reads the four outcomes the server names', () => {
    const rows = readInviteRows([
      { email: 'a@x.io', status: 'ok', outcome: 'invited', msg: 'added', invitation_delivered: true },
      { email: 'b@x.io', status: 'error', outcome: 'already_member', msg: 'exists' },
      { email: 'oops', status: 'error', outcome: 'invalid_email', msg: 'Invalid email: oops' },
      { email: 'c@x.io', status: 'error', outcome: 'failed', msg: 'failed' },
    ]);
    expect(rows.map((row) => row.outcome)).toEqual([
      'invited',
      'already_member',
      'invalid_email',
      'failed',
    ]);
    expect(rows[0]?.delivered).toBe(true);
    expect(rows[1]?.delivered).toBe(false);
  });

  it('falls back to `status` for a server that predates `outcome`', () => {
    const rows = readInviteRows([
      { email: 'a@x.io', status: 'ok', msg: 'added' },
      { email: 'b@x.io', status: 'error', msg: 'already exists' },
    ]);
    expect(rows.map((row) => row.outcome)).toEqual(['invited', 'failed']);
  });

  it('refuses an unknown outcome string rather than passing it through', () => {
    const rows = readInviteRows([{ email: 'a@x.io', status: 'ok', outcome: 'teleported' }]);
    expect(rows[0]?.outcome).toBe('invited');
  });

  it('is empty for a body that is not an array', () => {
    expect(readInviteRows({ error: 'emails is required' })).toEqual([]);
    expect(readInviteRows(undefined)).toEqual([]);
  });

  it('skips non-object entries instead of inventing rows for them', () => {
    expect(readInviteRows(['nope', null, { email: 'a@x.io', status: 'ok' }])).toHaveLength(1);
  });
});

describe('readInviteRowsFromError', () => {
  it('reads the rows out of the 400 body an EliteaApiError carries', () => {
    const error = {
      failure: {
        kind: 'http',
        status: 400,
        body: [
          { email: 'a@x.io', status: 'ok', outcome: 'invited', invitation_delivered: true },
          { email: 'b@x.io', status: 'error', outcome: 'already_member' },
        ],
      },
    };
    expect(readInviteRowsFromError(error).map((row) => row.email)).toEqual(['a@x.io', 'b@x.io']);
  });

  it('is empty for a network failure that carries no body', () => {
    expect(readInviteRowsFromError({ failure: { kind: 'network', message: 'boom' } })).toEqual([]);
    expect(readInviteRowsFromError(new Error('boom'))).toEqual([]);
    expect(readInviteRowsFromError(null)).toEqual([]);
  });
});

describe('summariseInviteRows', () => {
  const mixed = readInviteRows([
    { email: 'a@x.io', status: 'ok', outcome: 'invited', invitation_delivered: true },
    { email: 'b@x.io', status: 'ok', outcome: 'invited', invitation_delivered: true },
    { email: 'c@x.io', status: 'error', outcome: 'already_member' },
    { email: 'oops', status: 'error', outcome: 'invalid_email' },
    { email: 'd@x.io', status: 'error', outcome: 'failed' },
  ]);

  it('counts each outcome', () => {
    expect(summariseInviteRows(mixed)).toMatchObject({
      invited: 2,
      alreadyMember: 1,
      invalid: 1,
      failed: 1,
      delivered: true,
    });
  });

  it('reports delivered=false when any invited row sent no e-mail', () => {
    const rows = readInviteRows([
      { email: 'a@x.io', status: 'ok', outcome: 'invited', invitation_delivered: true },
      { email: 'b@x.io', status: 'ok', outcome: 'invited', invitation_delivered: false },
    ]);
    expect(summariseInviteRows(rows).delivered).toBe(false);
  });

  it('reports delivered=false for an empty batch rather than vacuously true', () => {
    expect(summariseInviteRows([]).delivered).toBe(false);
  });

  it('flags a batch that needs reading, and does not flag one that does not', () => {
    expect(hasInviteFailures(mixed)).toBe(true);
    expect(
      hasInviteFailures(readInviteRows([{ email: 'a@x.io', status: 'ok', outcome: 'invited' }])),
    ).toBe(false);
  });
});

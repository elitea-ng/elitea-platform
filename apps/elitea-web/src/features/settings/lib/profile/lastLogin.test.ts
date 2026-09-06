import { describe, expect, it } from 'vitest';

import { formatLastLogin, selectLastLogin } from './lastLogin';

/** The exact body a running stack returned for `GET /api/v2/social/authors/2`. */
const LIVE_BODY = [
  {
    id: 2,
    email: 'admin@client.local',
    name: 'Client Admin',
    last_login: 'Sun, 06 Sep 2026 14:36:57 GMT',
    suspended: false,
    avatar: null,
  },
];

describe('selectLastLogin', () => {
  it('reads the caller row out of a live production-handler body', () => {
    expect(selectLastLogin(LIVE_BODY, '2')).toBe('Sun, 06 Sep 2026 14:36:57 GMT');
  });

  it('matches across the id TYPE mismatch between the two endpoints', () => {
    // `/social/author` says `id: "2"` (string); `/social/authors/{id}` says
    // `id: 2` (number). A strict comparison would never match, and the row
    // would silently render blank for every user.
    expect(selectLastLogin([{ id: 2, last_login: 'x' }], '2')).toBe('x');
    expect(selectLastLogin([{ id: '2', last_login: 'x' }], '2')).toBe('x');
  });

  it('ignores other members of the project', () => {
    const rows = [
      { id: 9, last_login: 'someone else' },
      { id: 2, last_login: 'mine' },
    ];
    expect(selectLastLogin(rows, '2')).toBe('mine');
  });

  it('returns undefined when the deployment serves the spec shape (no last_login)', () => {
    // `SocialAuthorSummary` — what `v2.yaml` documents and the legacy handler
    // emits. The row must not throw and must not fabricate a timestamp.
    expect(selectLastLogin([{ id: 2, name: 'n', email: 'e' }], '2')).toBeUndefined();
  });

  it('returns undefined for a null, empty, absent or non-list body', () => {
    expect(selectLastLogin([{ id: 2, last_login: null }], '2')).toBeUndefined();
    expect(selectLastLogin([{ id: 2, last_login: '' }], '2')).toBeUndefined();
    expect(selectLastLogin([], '2')).toBeUndefined();
    expect(selectLastLogin(undefined, '2')).toBeUndefined();
    expect(selectLastLogin({ rows: LIVE_BODY }, '2')).toBeUndefined();
    expect(selectLastLogin([null, 'x', 3], '2')).toBeUndefined();
  });

  it('returns undefined when the viewer id is unknown', () => {
    expect(selectLastLogin(LIVE_BODY, undefined)).toBeUndefined();
    expect(selectLastLogin(LIVE_BODY, '')).toBeUndefined();
  });
});

describe('formatLastLogin', () => {
  it("renders the reference's `new Date(v).toLocaleString()`", () => {
    const raw = 'Sun, 06 Sep 2026 14:36:57 GMT';
    expect(formatLastLogin(raw)).toBe(new Date(raw).toLocaleString());
  });

  it('parses the RFC3339 form other emitters use', () => {
    const raw = '2026-09-06T14:36:57Z';
    expect(formatLastLogin(raw)).toBe(new Date(raw).toLocaleString());
  });

  it('renders an empty value rather than "Invalid Date" or a blank row', () => {
    // The reference always draws the row (`dateFormatter(last_login) || ''`),
    // so the page has the same number of rows on every deployment.
    expect(formatLastLogin(undefined)).toBe('');
    expect(formatLastLogin(null)).toBe('');
    expect(formatLastLogin('')).toBe('');
    expect(formatLastLogin('not a date')).toBe('');
  });
});

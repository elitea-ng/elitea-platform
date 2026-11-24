/**
 * Serialisation guards for the two admin CSV exports.
 *
 * Every property here is one a "does a file download?" assertion cannot see: a
 * mis-quoted comma still downloads, and a formula cell still downloads — it
 * just executes when the operator opens it.
 */
import { describe, expect, it } from 'vitest';

import { fetchAllPages, toCsvField } from './adminCsv';
import { buildAdminProjectsCsv } from './adminProjectsCsv';
import { buildAdminUsersCsv } from './adminUsersCsv';
import type { AdminProjectRow } from './api/adminProjectsApi';
import type { AdminUserRow } from './api/adminUsersApi';

function row(overrides: Partial<AdminUserRow> = {}): AdminUserRow {
  return {
    id: 1,
    name: 'Ada Admin',
    email: 'ada@example.com',
    last_login: '2026-08-01T10:00:00',
    suspended: false,
    is_admin: true,
    admin_roles: ['admin'],
    ...overrides,
  };
}

describe('toCsvField', () => {
  it('quotes and doubles per RFC 4180', () => {
    expect(toCsvField('plain')).toBe('plain');
    expect(toCsvField('Doe, Jane')).toBe('"Doe, Jane"');
    expect(toCsvField('Ada "Ace"')).toBe('"Ada ""Ace"""');
    expect(toCsvField('two\nlines')).toBe('"two\nlines"');
  });

  it('neutralises a cell Excel would execute as a formula', () => {
    // A display name arrives from SSO/SCIM, so it is attacker-controlled.
    expect(toCsvField('=HYPERLINK("http://evil","click")')).toBe(
      '"\'=HYPERLINK(""http://evil"",""click"")"',
    );
    expect(toCsvField('+1 555 0100')).toBe("'+1 555 0100");
    expect(toCsvField('-Minus')).toBe("'-Minus");
    expect(toCsvField('@handle')).toBe("'@handle");
  });
});

function project(overrides: Partial<AdminProjectRow> = {}): AdminProjectRow {
  return {
    id: 7,
    name: 'Alpha',
    owner_id: 3,
    owner_name: 'Ada Admin',
    admin_names: ['Bo Blocked', 'Cy Coder'],
    status: 'active',
    suspended: false,
    create_success: true,
    is_personal: false,
    ...overrides,
  };
}

describe('buildAdminProjectsCsv', () => {
  it('writes the reference page\u2019s five columns, admins joined into one cell', () => {
    const csv = buildAdminProjectsCsv([
      project(),
      project({ id: 8, name: 'Beta, Ltd', owner_name: '', admin_names: [], status: 'suspended' }),
    ]);

    expect(csv.split('\r\n')).toEqual([
      'Name,ID,Owner,Admins,Status',
      // The joined admin list contains a comma, so the CELL must be quoted —
      // unquoted it would shift Status into a column of its own.
      'Alpha,7,Ada Admin,"Bo Blocked, Cy Coder",Active',
      '"Beta, Ltd",8,,,Suspended',
    ]);
  });

  it('renders a failed provisioning run as its own status, not as Active', () => {
    expect(buildAdminProjectsCsv([project({ status: 'failed' })])).toContain(',Failed');
  });
});

describe('buildAdminUsersCsv', () => {
  it('writes the header and one CRLF-terminated line per user', () => {
    const csv = buildAdminUsersCsv([
      row({ admin_roles: ['admin', 'llm_manager'] }),
      row({ name: 'Bo Blocked', email: 'bo@example.com', last_login: null, suspended: true, admin_roles: [] }),
    ]);

    expect(csv.split('\r\n')).toEqual([
      'Name,Email,Last login,Status,Administration roles',
      'Ada Admin,ada@example.com,2026-08-01T10:00:00,Active,Platform administrator; LLM manager',
      'Bo Blocked,bo@example.com,Never,Suspended,None',
    ]);
  });

  it('is the header alone when nothing matches the filter', () => {
    expect(buildAdminUsersCsv([])).toBe('Name,Email,Last login,Status,Administration roles');
  });
});

describe('fetchAllPages', () => {
  /**
   * The regression this exists for: the walk used to request 500 rows and stop
   * on any page shorter than that. The admin handler ignores a `limit` over
   * 100 and serves its default 20, so the first page WAS short and the export
   * silently truncated to 20 rows. A fake that serves fewer rows than asked
   * reproduces exactly that; the MSW page handlers cannot, because they return
   * the whole fixture whatever `limit` says.
   */
  it('keeps walking when the server serves fewer rows than requested', async () => {
    const all = Array.from({ length: 57 }, (_, i) => i);
    const asked: Array<{ limit: number; offset: number }> = [];
    const result = await fetchAllPages((limit, offset) => {
      asked.push({ limit, offset });
      // The clamp: 20 rows however many were requested.
      return Promise.resolve({ rows: all.slice(offset, offset + 20), total: all.length });
    });

    expect(result.rows).toEqual(all);
    expect(result.truncated).toBe(false);
    // Offsets advance by what was RETURNED (20), not by what was requested.
    expect(asked.map((a) => a.offset)).toEqual([0, 20, 40]);
    expect(asked.every((a) => a.limit <= 100)).toBe(true);
  });

  it('stops on an empty page even when `total` is overstated', async () => {
    const result = await fetchAllPages((_limit, offset) =>
      Promise.resolve({ rows: offset === 0 ? [1, 2, 3] : [], total: 9_999 }),
    );

    expect(result.rows).toEqual([1, 2, 3]);
    expect(result.truncated).toBe(false);
  });

  it('reports truncation when a server that ignores `offset` hits the ceiling', async () => {
    const page = Array.from({ length: 100 }, (_, i) => i);
    const result = await fetchAllPages(() => Promise.resolve({ rows: page, total: 1_000_000 }));

    // Capped rather than looping forever — and the caller is TOLD, so the page
    // can say so instead of handing over a short file that looks complete.
    expect(result.truncated).toBe(true);
    expect(result.rows).toHaveLength(100_000);
  });
});

// @vitest-environment node
/**
 * TEMPORARY (remove in R3.6, issue 2537): the Roles page's level, derived
 * from the caller's roles and the roles that open `access`. Fed by the
 * server's own golden (`item-access.golden.json`), so the derivation cannot
 * silently make every caller view-only: super_admin and admin write,
 * security_auditor reads.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import type { AdminConsoleGrantingRole } from '@/shared/api/generated/model';

import { temporaryRolesLevel } from './temporaryRolesLink';

const GOLDEN = join(
  process.cwd(),
  '../../services/elitea-main/internal/api/v2/adminaccess/testdata/console/item-access.golden.json',
);

const granting = (JSON.parse(readFileSync(GOLDEN, 'utf8')) as { granting_roles: AdminConsoleGrantingRole[] })
  .granting_roles;

function builtIn(...names: string[]) {
  return names.map((name) => ({ name, built_in: true }));
}

describe('the temporary Roles level', () => {
  it('reads the access item from the golden', () => {
    expect(granting.length).toBeGreaterThan(0);
  });

  it('is write for super_admin and admin', () => {
    expect(temporaryRolesLevel(builtIn('super_admin'), granting)).toBe('write');
    expect(temporaryRolesLevel(builtIn('admin'), granting)).toBe('write');
  });

  it('is read for security_auditor', () => {
    expect(temporaryRolesLevel(builtIn('security_auditor'), granting)).toBe('read');
  });

  it('takes the highest level of the caller’s roles', () => {
    expect(temporaryRolesLevel(builtIn('security_auditor', 'admin'), granting)).toBe('write');
  });

  it('gives no level to a caller none of whose roles opens the item, so the server decides', () => {
    expect(temporaryRolesLevel(builtIn('llm_manager'), granting)).toBeUndefined();
    // A custom role named like a built-in one does not open what it opens.
    expect(temporaryRolesLevel([{ name: 'admin', built_in: false }], granting)).toBeUndefined();
  });
});

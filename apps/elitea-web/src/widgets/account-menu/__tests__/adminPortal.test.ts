import { describe, expect, it } from 'vitest';

import { adminPortalUrl, holdsAdministrationPermission } from '../lib/adminPortal';
import { versionLine } from '../lib/versionLine';

describe('holdsAdministrationPermission — who sees "Admin portal"', () => {
  it('is true for any granted administration permission', () => {
    expect(holdsAdministrationPermission([{ name: 'admin.users.view', enabled: true }])).toBe(true);
    // The server omits `enabled` on nothing today; an absent flag is not a refusal.
    expect(holdsAdministrationPermission([{ name: 'admin.roles.view' }])).toBe(true);
  });

  it('is false for an empty list, a disabled one, and anything that is not a list', () => {
    expect(holdsAdministrationPermission([])).toBe(false);
    expect(holdsAdministrationPermission([{ name: 'admin.users.view', enabled: false }])).toBe(false);
    expect(holdsAdministrationPermission([{ name: '', enabled: true }])).toBe(false);
    expect(holdsAdministrationPermission([null])).toBe(false);
    expect(holdsAdministrationPermission(undefined)).toBe(false);
    expect(holdsAdministrationPermission({ name: 'admin.users.view' })).toBe(false);
    expect(holdsAdministrationPermission('admin')).toBe(false);
  });
});

describe('adminPortalUrl', () => {
  it('is the console path on this origin, or on the given deployment', () => {
    expect(adminPortalUrl()).toBe('/admin/app/');
    expect(adminPortalUrl('')).toBe('/admin/app/');
    expect(adminPortalUrl('https://elitea.example')).toBe('https://elitea.example/admin/app/');
    expect(adminPortalUrl('https://elitea.example/')).toBe('https://elitea.example/admin/app/');
  });
});

describe('versionLine', () => {
  const labels = { client: 'Version', server: 'Server' };
  it('names the client, and the server once it is known', () => {
    expect(versionLine('1.72.3', undefined, labels)).toBe('Version 1.72.3');
    expect(versionLine('1.72.3', '1.72', labels)).toBe('Version 1.72.3 · Server 1.72');
  });
});

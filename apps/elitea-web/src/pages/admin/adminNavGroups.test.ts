/**
 * The navigation's permission names against the SERVER's registry
 * (ADR-0059 R0.16).
 *
 * `./adminPermissionRegistry.json` is written by a Go test
 * (`services/elitea-main/internal/api/v2/admin/admin_nav_registry_export_test.go`)
 * from the permission table and the Configuration sections, and that test
 * fails when the file is stale. This file fails when an item names a string
 * the registry does not have, so a drift fails on both sides.
 */
import { describe, expect, it } from 'vitest';

import { adminNavGroups } from './adminNavItems';
import registry from './adminPermissionRegistry.json';

const REGISTRY_NAMES = new Set(registry.names);

function navItem(id: string) {
  const item = adminNavGroups()
    .flatMap((group) => group.items)
    .find((candidate) => candidate.id === id);
  if (item === undefined) throw new Error(`no nav item ${id}`);
  return item;
}

describe('admin navigation permission names', () => {
  it('names only permissions of the server registry', () => {
    for (const group of adminNavGroups()) {
      for (const item of group.items) {
        for (const permission of item.anyPermission) {
          expect(
            REGISTRY_NAMES.has(permission),
            `nav item "${item.id}" gates on "${permission}", which is no row of the permission table`,
          ).toBe(true);
        }
      }
    }
  });

  it('shows Configuration to exactly the names its three routes admit', () => {
    expect([...navItem('configuration').anyPermission].sort()).toEqual(
      [...registry.configuration_route_permissions].sort(),
    );
  });

  it('reads a registry file that is not empty', () => {
    // An empty export would make the first test pass for the wrong reason.
    expect(registry.names.length).toBeGreaterThan(50);
    expect(registry.declared_role_sets.llm_manager.length).toBeGreaterThan(0);
  });
});

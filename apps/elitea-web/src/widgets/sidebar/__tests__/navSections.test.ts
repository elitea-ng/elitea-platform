import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSION_GROUPS } from '@/shared/lib/permissions';

import { computeIsSelectedProjectPublic, navSections, selectedNavItem, visibleNavSections } from '../lib/navSections';

describe('navSections (SHELL-001..009)', () => {
  it('lists exactly the 10 items across 3 groups, in old-app order', () => {
    const sections = navSections();
    expect(sections).toHaveLength(3);
    expect(sections[0]?.items.map((i) => i.value)).toEqual(['chat', 'agents', 'pipelines']);
    expect(sections[1]?.items.map((i) => i.value)).toEqual([
      'skills',
      'toolkits',
      'mcps',
      'credentials',
      'applications',
    ]);
    expect(sections[2]?.items.map((i) => i.value)).toEqual(['artifacts', 'marketplace']);
  });
});

describe('visibleNavSections (SHELL-010)', () => {
  it('keeps every item when every required permission is held', () => {
    const all = new Set(Object.values(PERMISSION_GROUPS).flat());
    const visible = visibleNavSections(navSections(), all);
    const totalItems = visible.reduce((count, section) => count + section.items.length, 0);
    expect(totalItems).toBe(10);
  });

  it('drops a gated item whose permission is missing, without dropping ungated siblings', () => {
    // No permissions at all: chat/agents/pipelines/credentials/toolkits/artifacts/mcps
    // are all gated (mcps reuses the toolkits group) and disappear; skills,
    // applications and the Marketplace have no PERMISSION_GROUPS entry and
    // always survive.
    const visible = visibleNavSections(navSections(), new Set());
    const values = visible.flatMap((section) => section.items.map((item) => item.value));
    expect(values).toEqual(['skills', 'applications', 'marketplace']);
  });

  it('drops an entire EMPTY section object, not just its (already-filtered) items', () => {
    const visible = visibleNavSections(navSections(), new Set());
    // Group 1 (chat/agents/pipelines) is entirely gated; group 2 survives via
    // its two UNGATED items (skills, applications) and group 3 via the
    // Marketplace — none has a PERMISSION_GROUPS entry. The gated section
    // must be removed from the array (2 sections left, not 3 with one holding
    // an empty `items: []`) — proven by asserting NO
    // section has an empty items array, not merely that 'chat' is absent
    // from the flattened item list (a mutation that drops item-level
    // filtering but leaves stray empty sections would still pass that
    // weaker check).
    expect(visible).toHaveLength(2);
    expect(visible.every((section) => section.items.length > 0)).toBe(true);
    expect(visible.some((section) => section.items.some((item) => item.value === 'chat'))).toBe(false);
  });

  it('grants a gated item when its permission is held', () => {
    const visible = visibleNavSections(navSections(), new Set(PERMISSION_GROUPS.chat));
    expect(visible.flatMap((s) => s.items.map((i) => i.value))).toContain('chat');
  });

  it('R3: hides "skills" when isSelectedProjectPublic is true, in addition to the permission check', () => {
    const all = new Set(Object.values(PERMISSION_GROUPS).flat());
    const visible = visibleNavSections(navSections(), all, { isSelectedProjectPublic: true });
    const values = visible.flatMap((section) => section.items.map((item) => item.value));
    expect(values).not.toContain('skills');
    // Ungated/unrelated siblings still survive.
    expect(values).toContain('toolkits');
  });

  it('R3: shows "skills" when isSelectedProjectPublic is false (default) even with no options passed', () => {
    const all = new Set(Object.values(PERMISSION_GROUPS).flat());
    const visible = visibleNavSections(navSections(), all);
    const values = visible.flatMap((section) => section.items.map((item) => item.value));
    expect(values).toContain('skills');
  });
});

describe('computeIsSelectedProjectPublic (R3)', () => {
  beforeEach(() => {
    resetConfigForTests();
    vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
    vi.stubEnv('VITE_BASE_URI', '/app/');
    vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    resetConfigForTests();
  });

  it('is true when the selected project id matches the runtime public project id', () => {
    expect(computeIsSelectedProjectPublic('11')).toBe(true);
  });

  it('is false when the selected project id differs from the public project id', () => {
    expect(computeIsSelectedProjectPublic('2')).toBe(false);
  });

  it('is false when no project is selected', () => {
    expect(computeIsSelectedProjectPublic(undefined)).toBe(false);
  });
});

describe('selectedNavItem', () => {
  it('matches the item whose url is a prefix of the pathname', () => {
    const items = navSections()[0]?.items ?? [];
    expect(selectedNavItem('/chat/abc-123', items)).toBe('chat');
    expect(selectedNavItem('/agents/latest/agent-1', items)).toBe('agents');
  });

  /*
   * The Catalog (Agent HUB) is the Marketplace (ADR-0051): a primary row.
   * Its listing pages sit under `/marketplace`, so the prefix scan keeps the
   * row selected on them; the two old addresses redirect and match nothing.
   */
  it('matches /marketplace and its listing pages to the Marketplace row', () => {
    const items = navSections().flatMap((section) => section.items);
    expect(selectedNavItem('/marketplace', items)).toBe('marketplace');
    expect(selectedNavItem('/marketplace/agents/42', items)).toBe('marketplace');
  });

  it('returns undefined for a pathname matching no nav item', () => {
    const items = navSections()[0]?.items ?? [];
    expect(selectedNavItem('/mode-switch', items)).toBeUndefined();
  });
});

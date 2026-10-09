import { describe, expect, it } from 'vitest';

import type { Toolkit } from '@/entities/toolkit';

import { buildInstanceItems } from './ToolMenuSections';

function toolkit(overrides: Partial<Toolkit> = {}): Toolkit {
  return { id: '1', name: 'Jira Cloud', type: 'jira', ...overrides };
}

/**
 * elitea_issues #5296 — "Toolkit search on Agents page displays names
 * without spaces - inconsistent with Toolkit page formatting". Both surfaces
 * render the SAME `toolkit.name` field verbatim (no slug/no-space
 * transform anywhere in `buildInstanceItems`), so they cannot diverge.
 */
describe('buildInstanceItems — elitea_issues 5296 label consistency', () => {
  it('renders the toolkit name verbatim, spaces included — not a slugified/no-space form', () => {
    const items = buildInstanceItems([toolkit({ name: 'Jira Cloud' })], new Set(), false, '', undefined, () => {});
    expect(items).toHaveLength(1);
    expect(items[0]?.label).toBe('Jira Cloud');
  });

  it('matches search case-insensitively against the same verbatim name', () => {
    const items = buildInstanceItems([toolkit({ name: 'Jira Cloud' })], new Set(), false, 'jira cloud', undefined, () => {});
    expect(items).toHaveLength(1);
    expect(items[0]?.label).toBe('Jira Cloud');
  });

  it('matches the search against the description too, like the server', () => {
    const items = buildInstanceItems([toolkit({ name: 'Plain', description: 'Reads the NEEDLE' })], new Set(), false, 'needle', undefined, () => {});
    expect(items).toHaveLength(1);
  });

  it('never lists an `application` row (an agent-as-tool link), whichever section asks', () => {
    const link = toolkit({ id: '9', name: 'Cedar inspection child', type: 'application' });
    expect(buildInstanceItems([link], new Set(), false, '', undefined, () => {})).toHaveLength(0);
    expect(buildInstanceItems([link], new Set(), true, '', undefined, () => {})).toHaveLength(0);
  });
});

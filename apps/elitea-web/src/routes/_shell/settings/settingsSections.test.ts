import { describe, expect, it } from 'vitest';

import { buildSettingsSections, isTabHidden } from './settingsSections';

const ALL_OPEN = {
  isPublicProject: false,
  isPersonalProject: false,
  analyticsVisible: true,
  usageVisible: true,
};

function projectTabIds(gates: Parameters<typeof buildSettingsSections>[0]): string[] {
  return buildSettingsSections(gates)[0]!.tabs.map((tab) => tab.id);
}

describe('buildSettingsSections — PROJECT', () => {
  it('opens with General, which this port did not have at all', () => {
    // The reference's first row and its DEFAULT_TAB
    // (`pages/settings/index.jsx:56-60,130`); a live deployment lands here.
    expect(projectTabIds(ALL_OPEN)[0]).toBe('project-general');
  });

  it('matches what a live deployment shows in a personal project, plus the new Webhooks tab', () => {
    // Measured read-only on next.elitea.ai, signed in, project "Private":
    // General, AI Providers, Project Context, Secrets, Analytics, Usage.
    // No Service Prompts, no Environment, no Users. `webhooks` is NOT part of
    // that measurement — it has no equivalent in the reference app at all
    // (the outbound webhook registry is a capability this platform added,
    // see webhook/handler.go's file header) — it is added by #876 right
    // after Secrets, the other project-scoped credential-shaped tab.
    expect(
      projectTabIds({ ...ALL_OPEN, isPersonalProject: true }),
    ).toEqual([
      'project-general',
      'model-configuration',
      'project-params',
      'secrets',
      'webhooks',
      'analytics',
      'usage',
    ]);
  });

  it('shows Service Prompts and Environment ONLY in the public project', () => {
    // Both screens gate every query on `useIsPublicProject`, so anywhere else
    // they render an empty body: no rows, no error, no request. Measured on a
    // fresh install — server `public_project_id: 1`, image
    // `VITE_PUBLIC_PROJECT_ID=99`, user in personal project 2 — both drew a
    // blank page.
    expect(projectTabIds(ALL_OPEN)).not.toContain('prompts');
    expect(projectTabIds(ALL_OPEN)).not.toContain('environment');

    const publicTabs = projectTabIds({ ...ALL_OPEN, isPublicProject: true });
    expect(publicTabs).toContain('prompts');
    expect(publicTabs).toContain('environment');
  });

  it('hides Project Context in the public project, the mirror of that rule', () => {
    expect(projectTabIds({ ...ALL_OPEN, isPublicProject: true })).not.toContain('project-params');
    expect(projectTabIds(ALL_OPEN)).toContain('project-params');
  });

  it('hides Users in the caller’s own personal project', () => {
    // A one-person project has no membership to manage.
    expect(projectTabIds({ ...ALL_OPEN, isPersonalProject: true })).not.toContain('users');
    expect(projectTabIds(ALL_OPEN)).toContain('users');
  });

  it('keeps the two platform-flag gates that already worked', () => {
    const tabs = projectTabIds({ ...ALL_OPEN, analyticsVisible: false, usageVisible: false });
    expect(tabs).not.toContain('analytics');
    expect(tabs).not.toContain('usage');
  });
});

describe('buildSettingsSections — PERSONAL', () => {
  it('is the reference order, ungated', () => {
    const personal = buildSettingsSections(ALL_OPEN)[1]!;
    expect(personal.section).toBe('PERSONAL');
    expect(personal.tabs.map((tab) => tab.id)).toEqual([
      'profile',
      'preferences',
      'ai-personality',
      'memory',
      'tokens',
      'notifications',
    ]);
  });
});

describe('isTabHidden', () => {
  it('reports the same three tabs the drawer drops, so a URL cannot reach them', () => {
    // Dropping a drawer row stops it being clickable; it does not stop
    // `/settings/prompts` being typed or bookmarked.
    expect(isTabHidden('prompts', ALL_OPEN)).toBe(true);
    expect(isTabHidden('environment', ALL_OPEN)).toBe(true);
    expect(isTabHidden('users', { ...ALL_OPEN, isPersonalProject: true })).toBe(true);
    expect(isTabHidden('project-params', { ...ALL_OPEN, isPublicProject: true })).toBe(true);
  });

  it('leaves every visible tab alone', () => {
    for (const tab of ['project-general', 'model-configuration', 'secrets', 'analytics', 'profile']) {
      expect(isTabHidden(tab, ALL_OPEN), tab).toBe(false);
    }
    expect(isTabHidden('prompts', { ...ALL_OPEN, isPublicProject: true })).toBe(false);
    expect(isTabHidden('users', ALL_OPEN)).toBe(false);
  });

  it('does not redirect when there is no tab segment yet', () => {
    expect(isTabHidden(undefined, ALL_OPEN)).toBe(false);
  });
});

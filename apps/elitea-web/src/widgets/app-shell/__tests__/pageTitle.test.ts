import { describe, expect, it } from 'vitest';

import { derivePageTitle } from '../lib/pageTitle';

describe('derivePageTitle', () => {
  it('falls back to the project name alone for the personal-space root', () => {
    expect(derivePageTitle('/', '', 'My Project')).toBe('My Project');
  });

  it('section - project, when no ?name= is present', () => {
    expect(derivePageTitle('/agents', '', 'My Project')).toBe('Agents - My Project');
  });

  it('section: name - project, when ?name= is present', () => {
    expect(derivePageTitle('/chat', 'Weekly sync', 'My Project')).toBe('Chat: Weekly sync - My Project');
  });

  it('titles /marketplace as the Marketplace, not as a section of its own kind', () => {
    expect(derivePageTitle('/marketplace', '', 'My Project')).toBe('Marketplace - My Project');
  });

  it('settings: bare tab -> "Settings - project"', () => {
    expect(derivePageTitle('/settings', '', 'My Project')).toBe('Settings - My Project');
  });

  it('settings: known-tab special casing (personalization/notifications)', () => {
    expect(derivePageTitle('/settings/personalization', '', 'My Project')).toBe('Settings: Personalization - My Project');
    expect(derivePageTitle('/settings/notifications', '', 'My Project')).toBe('Settings: Notifications - My Project');
  });

  it('settings: an arbitrary tab is titled verbatim', () => {
    expect(derivePageTitle('/settings/secrets', '', 'My Project')).toBe('Settings: secrets - My Project');
  });

  it('an unrecognised route falls back to the project name alone', () => {
    expect(derivePageTitle('/mode-switch', '', 'My Project')).toBe('My Project');
  });

  it('tolerates an unknown project name (renders the empty string, not "undefined")', () => {
    expect(derivePageTitle('/agents', '', undefined)).toBe('Agents - ');
  });

  // R5 regression: the old app (`useBrowserPageTitle.js`) titles every
  // tabbed section with `params.tab`, not just `/settings`. Every section
  // below has a real `routes/_shell/<section>/$tab.tsx`, so the tab is just
  // as reachable from the pathname as `/settings`'s tab already is.
  describe('per-section tab fallback (parity with the /settings implementation)', () => {
    it.each([
      ['/agents/mine', 'Agents: mine - My Project'],
      ['/pipelines/mine', 'Pipelines: mine - My Project'],
      ['/toolkits/mine', 'Toolkits: mine - My Project'],
      ['/mcps/mine', 'MCPs: mine - My Project'],
      ['/credentials/mine', 'Credentials: mine - My Project'],
      ['/skills/mine', 'Skills: mine - My Project'],
      ['/apps/mine', 'Applications: mine - My Project'],
      ['/user-public/agents', 'User public: agents - My Project'],
    ])('%s -> %s', (pathname, expected) => {
      expect(derivePageTitle(pathname, '', 'My Project')).toBe(expected);
    });

    it('a bare (tab-less) section pathname still falls back to "section - project"', () => {
      expect(derivePageTitle('/toolkits', '', 'My Project')).toBe('Toolkits - My Project');
      expect(derivePageTitle('/toolkits/', '', 'My Project')).toBe('Toolkits - My Project');
    });

    it('?name= still takes priority over the pathname tab', () => {
      expect(derivePageTitle('/toolkits/mine', 'My Toolkit', 'My Project')).toBe('Toolkits: My Toolkit - My Project');
    });

    it('sections with no $tab route (chat/marketplace/artifacts/help-center) are unaffected', () => {
      expect(derivePageTitle('/marketplace', '', 'My Project')).toBe('Marketplace - My Project');
      expect(derivePageTitle('/artifacts', '', 'My Project')).toBe('Artifacts - My Project');
      expect(derivePageTitle('/help-center', '', 'My Project')).toBe('Help Center - My Project');
      expect(derivePageTitle('/chat', '', 'My Project')).toBe('Chat - My Project');
    });
  });
});

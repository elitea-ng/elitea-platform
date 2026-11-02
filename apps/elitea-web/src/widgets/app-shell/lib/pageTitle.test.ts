import { describe, expect, it } from 'vitest';

import { derivePageTitle, pageSection } from './pageTitle';

describe('derivePageTitle', () => {
  it('returns project name alone for unknown path', () => {
    expect(derivePageTitle('/unknown', '', 'MyProject')).toBe('MyProject');
  });

  it('returns empty string when no match and no project', () => {
    expect(derivePageTitle('/unknown', '', undefined)).toBe('');
  });

  it('derives settings title with no tab', () => {
    expect(derivePageTitle('/settings', '', 'Proj')).toBe('Settings - Proj');
  });

  it('derives settings title with known tab', () => {
    expect(derivePageTitle('/settings/personalization', '', 'Proj')).toBe('Settings: Personalization - Proj');
    expect(derivePageTitle('/settings/notifications', '', 'Proj')).toBe('Settings: Notifications - Proj');
  });

  it('derives settings title with unknown tab (passthrough)', () => {
    expect(derivePageTitle('/settings/custom-tab', '', 'Proj')).toBe('Settings: custom-tab - Proj');
  });

  it('derives chat section without searchName', () => {
    expect(derivePageTitle('/chat', '', 'Proj')).toBe('Chat - Proj');
  });

  it('derives section with searchName', () => {
    expect(derivePageTitle('/chat/123', 'My Conv', 'Proj')).toBe('Chat: My Conv - Proj');
  });

  it('derives agents with tab', () => {
    expect(derivePageTitle('/agents/create', '', 'Proj')).toBe('Agents: create - Proj');
  });

  it('agents-hub does not match agents prefix', () => {
    expect(derivePageTitle('/agents-hub', '', 'Proj')).toBe('Agent HUB - Proj');
  });

  it('derives pipelines with tab', () => {
    expect(derivePageTitle('/pipelines/edit', '', 'Proj')).toBe('Pipelines: edit - Proj');
  });

  it('derives toolkits without tab', () => {
    expect(derivePageTitle('/toolkits', '', 'Proj')).toBe('Toolkits - Proj');
  });

  it('derives mcps with tab', () => {
    expect(derivePageTitle('/mcps/config', '', 'P')).toBe('MCPs: config - P');
  });

  it('derives credentials, artifacts, apps', () => {
    expect(derivePageTitle('/credentials/new', '', 'P')).toBe('Credentials: new - P');
    expect(derivePageTitle('/artifacts', '', 'P')).toBe('Artifacts - P');
    expect(derivePageTitle('/apps/list', '', 'P')).toBe('Applications: list - P');
  });

  it('derives help-center (no tab support)', () => {
    expect(derivePageTitle('/help-center/faq', '', 'P')).toBe('Help Center - P');
  });

  it('handles undefined projectName gracefully', () => {
    expect(derivePageTitle('/chat', 'Conv', undefined)).toBe('Chat: Conv - ');
  });
});

describe('pageSection', () => {
  it('names the section and links its list', () => {
    expect(pageSection('/agents/12')).toEqual({ label: 'Agents', to: '/agents' });
    expect(pageSection('/agents-hub')).toEqual({ label: 'Agent HUB', to: '/agents-hub' });
  });

  it('names the chat and the toolkits as their sidebar rows do', () => {
    expect(pageSection('/chat/72')).toEqual({ label: 'Chats', to: '/chat' });
    expect(pageSection('/toolkits/all')).toEqual({ label: 'Toolkits & Indexes', to: '/toolkits' });
    // The document title keeps the old wording.
    expect(derivePageTitle('/chat', '', 'P')).toBe('Chat - P');
  });

  it('treats every settings tab as Settings', () => {
    expect(pageSection('/settings/notifications')).toEqual({ label: 'Settings', to: '/settings' });
  });

  it('is undefined outside every known section', () => {
    expect(pageSection('/unknown')).toBeUndefined();
  });
});

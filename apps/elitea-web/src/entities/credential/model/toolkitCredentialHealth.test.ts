/**
 * #937. The verdict half of the attached-toolkit credential warning. Each
 * case below is the shape a real toolkit stores, not an invented one:
 * `toolkits.p13-credential-warnings.spec.ts` seeds exactly
 * `settings.github_configuration = {elitea_title, private: false}`.
 */
import { describe, expect, it } from 'vitest';

import { collectCredentialTitles, isToolkitCredentialMissing, readToolkitCredentialReference } from './toolkitCredentialHealth';

describe('readToolkitCredentialReference', () => {
  it('reads the `{elitea_title, private}` pair a toolkit stores, and names the credential type from the key', () => {
    expect(readToolkitCredentialReference({ github_configuration: { elitea_title: 'prod-gh', private: false }, repository: 'a/b' })).toEqual({
      eliteaTitle: 'prod-gh',
      credentialType: 'github',
      isPrivate: false,
    });
  });

  it('carries the private flag, which decides WHICH project is asked', () => {
    expect(readToolkitCredentialReference({ jira_configuration: { elitea_title: 'mine', private: true } })?.isPrivate).toBe(true);
  });

  it('answers null for settings that reference no credential at all', () => {
    expect(readToolkitCredentialReference({ url: 'https://example.invalid' })).toBeNull();
    expect(readToolkitCredentialReference(undefined)).toBeNull();
    expect(readToolkitCredentialReference('not-an-object')).toBeNull();
    expect(readToolkitCredentialReference({ github_configuration: { elitea_title: '   ' } })).toBeNull();
  });

  it('answers null for the picker MODES, which reference nothing that could be missing', () => {
    for (const mode of ['Manual_Title', 'Create_Personal_Title', 'Create_Project_Title']) {
      expect(readToolkitCredentialReference({ github_configuration: { elitea_title: mode } })).toBeNull();
    }
  });
});

describe('collectCredentialTitles', () => {
  it('collects both places a saved title can sit', () => {
    const titles = collectCredentialTitles([{ elitea_title: 'top' }, { data: { title: 'nested' } }, {}]);
    expect([...titles].sort()).toEqual(['nested', 'top']);
  });
});

describe('isToolkitCredentialMissing', () => {
  const reference = { eliteaTitle: 'gone', credentialType: 'github', isPrivate: false } as const;

  it('is true only once a settled read has shown the title is absent', () => {
    expect(isToolkitCredentialMissing(reference, new Set(['other']))).toBe(true);
  });

  it('is false while there is no verdict — an unread list is not evidence of a missing credential', () => {
    expect(isToolkitCredentialMissing(reference, undefined)).toBe(false);
  });

  it('is false when the credential resolves, and when there is no reference to resolve', () => {
    expect(isToolkitCredentialMissing(reference, new Set(['gone']))).toBe(false);
    expect(isToolkitCredentialMissing(null, new Set<string>())).toBe(false);
  });
});

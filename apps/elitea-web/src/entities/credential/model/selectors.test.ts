import { describe, expect, it } from 'vitest';

import {
  credentialDisplayName,
  credentialScope,
  credentialUrl,
  providerDisplayName,
  sortCredentialsPinnedFirst,
} from './selectors';
import type { Credential } from './types';

const credential = (overrides: Partial<Credential> = {}): Credential => ({
  id: '1',
  type: 'openai',
  ...overrides,
});

describe('providerDisplayName', () => {
  it('strips an integration_ prefix', () => {
    expect(providerDisplayName('integration_github')).toBe('Github');
  });

  it('strips everything through the last Provider_ occurrence', () => {
    expect(providerDisplayName('AzureOpenAIProvider_gpt4')).toBe('Gpt4');
  });

  it('replaces underscores with spaces and capitalizes', () => {
    expect(providerDisplayName('azure_open_ai')).toBe('Azure open ai');
  });

  it('leaves a plain type name capitalized', () => {
    expect(providerDisplayName('openai')).toBe('Openai');
  });
});

describe('credentialDisplayName', () => {
  it('prefers label', () => {
    expect(credentialDisplayName(credential({ label: 'My Cred', eliteaTitle: 'x', data: { title: 'y' } }))).toBe(
      'My Cred',
    );
  });

  it('falls back to eliteaTitle when label is absent', () => {
    expect(credentialDisplayName(credential({ eliteaTitle: 'Elitea Title', data: { title: 'y' } }))).toBe(
      'Elitea Title',
    );
  });

  it('falls back to data.title when label and eliteaTitle are absent', () => {
    expect(credentialDisplayName(credential({ data: { title: 'Data Title' } }))).toBe('Data Title');
  });

  it('falls back to the provider display name derived from type', () => {
    expect(credentialDisplayName(credential({ type: 'integration_github' }))).toBe('Github');
  });

  it('treats a null label or title (the wire’s unlabelled credential) as absent, without throwing', () => {
    const unlabelled = { ...credential({ type: 'openai' }), label: null, eliteaTitle: null } as unknown as Credential;
    expect(credentialDisplayName(unlabelled)).toBe('Openai');
  });

  it('treats a blank label as absent', () => {
    expect(credentialDisplayName(credential({ label: '   ', type: 'openai' }))).toBe('Openai');
  });
});

describe('credentialUrl', () => {
  it('prefers base_url', () => {
    expect(credentialUrl(credential({ data: { base_url: 'https://a', url: 'https://b' } }))).toBe('https://a');
  });

  it('falls back to url when base_url is absent', () => {
    expect(credentialUrl(credential({ data: { url: 'https://b' } }))).toBe('https://b');
  });

  it('returns an empty string when neither is present', () => {
    expect(credentialUrl(credential())).toBe('');
  });

  it('falls back to url when base_url is an empty string', () => {
    expect(credentialUrl(credential({ data: { base_url: '', url: 'https://b' } }))).toBe('https://b');
  });
});

describe('credentialScope', () => {
  it('is Local when projectId is present and non-empty', () => {
    expect(credentialScope(credential({ projectId: 'p1' }))).toBe('Local');
  });

  it('is Inherited when projectId is absent', () => {
    expect(credentialScope(credential())).toBe('Inherited');
  });

  it('is Inherited when projectId is an empty string', () => {
    expect(credentialScope(credential({ projectId: '' }))).toBe('Inherited');
  });
});

describe('sortCredentialsPinnedFirst', () => {
  it('puts pinned credentials first, preserving relative order otherwise', () => {
    const a = credential({ id: 'a', isPinned: false });
    const b = credential({ id: 'b', isPinned: true });
    const c = credential({ id: 'c', isPinned: false });
    expect(sortCredentialsPinnedFirst([a, b, c]).map((cred) => cred.id)).toEqual(['b', 'a', 'c']);
  });

  it('does not mutate the input', () => {
    const list = [credential({ id: 'a' }), credential({ id: 'b', isPinned: true })];
    const copy = [...list];
    sortCredentialsPinnedFirst(list);
    expect(list).toEqual(copy);
  });
});

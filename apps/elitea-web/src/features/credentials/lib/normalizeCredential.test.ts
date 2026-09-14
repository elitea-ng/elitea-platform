import { describe, expect, it } from 'vitest';

import { normalizeCredential, normalizeCredentialPage } from './normalizeCredential';

describe('normalizeCredential', () => {
  it('preserves the authorization UUID separately from the row id', () => {
    expect(normalizeCredential({ id: 27, uuid: 'credential-uuid', type: 'openapi' }))
      .toEqual({ id: '27', uuid: 'credential-uuid', type: 'openapi' });
  });
  it('maps every wire field to its camelCase entity counterpart', () => {
    const result = normalizeCredential({
      uid: 'u1',
      id: 5,
      type: 'openai',
      data: { base_url: 'https://x' },
      elitea_title: 'My Cred',
      label: 'Label',
      shared: true,
      section: 'llm',
      project_id: 42,
      is_pinned: true,
    });
    expect(result).toEqual({
      id: 'u1',
      type: 'openai',
      uid: 'u1',
      data: { base_url: 'https://x' },
      eliteaTitle: 'My Cred',
      label: 'Label',
      shared: true,
      section: 'llm',
      projectId: '42',
      isPinned: true,
    });
  });

  it('falls back to id when uid is absent', () => {
    const result = normalizeCredential({ id: 9, type: 'azure' });
    expect(result.id).toBe('9');
    expect(result.uid).toBeUndefined();
  });

  it('omits optional fields entirely when the wire object omits them (exactOptionalPropertyTypes safety)', () => {
    const result = normalizeCredential({ type: 'openai' });
    expect(Object.keys(result).sort()).toEqual(['id', 'type']);
  });
});

describe('normalizeCredentialPage', () => {
  it('normalizes items and the shared sub-page', () => {
    const result = normalizeCredentialPage({
      items: [{ uid: 'a', type: 'openai' }],
      total: 1,
      limit: 20,
      offset: 0,
      shared: { items: [{ uid: 'b', type: 'azure' }], total: 1 },
    });
    expect(result.items).toEqual([{ id: 'a', uid: 'a', type: 'openai' }]);
    expect(result.shared).toEqual({ items: [{ id: 'b', uid: 'b', type: 'azure' }], total: 1 });
  });

  it('omits shared when the wire response has none', () => {
    const result = normalizeCredentialPage({ items: [], total: 0, limit: 20, offset: 0 });
    expect(result.shared).toBeUndefined();
  });
});

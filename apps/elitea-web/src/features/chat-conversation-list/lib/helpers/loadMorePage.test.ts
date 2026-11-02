import { describe, expect, it } from 'vitest';

import { mergeLoadMorePage } from './loadMorePage';

describe('mergeLoadMorePage', () => {
  // DEFECT: this file holds a second `toConversation`. It dropped `authorId`
  // and `name`. The row menu then denied Delete and Edit on every row that
  // the user paged in, including the user's own rows.
  it('keeps the author id and the name of a paged-in conversation', () => {
    const merged = mergeLoadMorePage({ conversations: [], offset: 0, total: 2 }, { conversations: [{ id: 'c9', name: 'My chat', authorId: '7' }] }, new Set());

    expect(merged.conversations).toHaveLength(1);
    expect(merged.conversations[0]).toMatchObject({ id: 'c9', name: 'My chat', authorId: '7' });
  });

  it('leaves the author id out when the page does not carry one', () => {
    const merged = mergeLoadMorePage({ conversations: [], offset: 0, total: 2 }, { conversations: [{ id: 'c9' }] }, new Set());

    expect(merged.conversations[0]).not.toHaveProperty('authorId');
    expect(merged.conversations[0]).toMatchObject({ id: 'c9', name: '', isPrivate: true });
  });

  it('drops a pinned or already present conversation and advances the offset', () => {
    const merged = mergeLoadMorePage({ conversations: [{ id: 'c1', name: 'c1', isPrivate: true }], offset: 1, total: 3 }, { conversations: [{ id: 'c1' }, { id: 'c2' }] }, new Set(['c2']));

    expect(merged.conversations.map((c) => c.id)).toEqual(['c1']);
    expect(merged.offset).toBe(3);
    expect(merged.exhausted).toBe(true);
  });
});

import { describe, expect, it } from 'vitest';

import { mergeLoadMorePage, resyncBucket, resyncLimit } from './loadMorePage';

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

  // Issue 3030: rows the bucket held were deleted elsewhere, so the rows
  // behind them moved up past the offset. Ending the bucket there left them
  // unreachable until a reload.
  it('does not end a bucket on an empty page whose total lies above the offset, and asks for a re-read', () => {
    const bucket = { conversations: [{ id: 'c1', name: 'c1', isPrivate: true }], offset: 10, total: 14 };
    const merged = mergeLoadMorePage(bucket, { conversations: [], total: 14 }, new Set());

    expect(merged.exhausted).toBe(false);
    expect(merged.refetch).toBe(true);
    expect(merged.conversations).toBe(bucket.conversations);
    expect(merged.offset).toBe(10);
  });

  it('asks for a re-read when the bucket total fell since the offset was taken', () => {
    const merged = mergeLoadMorePage({ conversations: [], offset: 10, total: 14 }, { conversations: [], total: 9 }, new Set());

    expect(merged.refetch).toBe(true);
    expect(merged.exhausted).toBe(false);
    expect(merged.total).toBe(14);
  });

  it('ends the bucket on an empty page at the total, or on an empty page with no total', () => {
    const atTotal = mergeLoadMorePage({ conversations: [], offset: 10, total: 10 }, { conversations: [], total: 10 }, new Set());
    const noTotal = mergeLoadMorePage({ conversations: [], offset: 10 }, { conversations: [] }, new Set());

    expect(atTotal).toMatchObject({ exhausted: true, refetch: false });
    expect(noTotal).toMatchObject({ exhausted: true, refetch: false });
  });

  it('keeps a bucket with no total open while its pages carry rows', () => {
    const merged = mergeLoadMorePage({ conversations: [], offset: 0 }, { conversations: [{ id: 'c1' }] }, new Set());

    expect(merged).toMatchObject({ exhausted: false, refetch: false, offset: 1 });
  });
});

describe('resyncBucket', () => {
  it('replaces the held rows with the re-read window, keeping a held row’s fields and dropping the rows that left', () => {
    const bucket = {
      conversations: [
        { id: 'gone', name: 'gone', isPrivate: true },
        { id: 'kept', name: 'old name', isPrivate: false, folderId: '4' },
      ],
      offset: 2,
      total: 3,
    };
    const resynced = resyncBucket(bucket, { conversations: [{ id: 'kept', name: 'new name', isPrivate: false }, { id: 'older' }, { id: 'pinned' }], total: 3 }, new Set(['pinned']));

    expect(resynced.conversations.map((c) => c.id)).toEqual(['kept', 'older']);
    expect(resynced.conversations[0]).toMatchObject({ name: 'new name', folderId: '4' });
    expect(resynced).toMatchObject({ offset: 3, total: 3, exhausted: true });
  });

  it('reads what the bucket holds and one more page', () => {
    expect(resyncLimit({ conversations: [{ id: 'a', name: 'a', isPrivate: true }] }, 10)).toBe(11);
  });
});

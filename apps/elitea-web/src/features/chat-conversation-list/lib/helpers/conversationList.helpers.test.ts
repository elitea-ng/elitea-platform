import { describe, expect, it } from 'vitest';

import type { Conversation } from '@/entities/conversation';

import { pinnedFoldersFirst, redistributeConversationsIntoGroups, sortConversations } from './conversationList.helpers';
import type { ConversationGroup } from './conversationList.helpers';

function mkConv(overrides: Partial<Conversation> & { readonly id: string }): Conversation {
  return { name: overrides.id, isPrivate: true, ...overrides };
}

type Group = ConversationGroup<Conversation> & { readonly total?: number; readonly offset?: number };

describe('redistributeConversationsIntoGroups', () => {
  it('keeps a known id in its original group even after the fetch reorders the flat list', () => {
    const prevGroups = [
      { name: 'today', conversations: [mkConv({ id: 'a' })] },
      { name: 'older', conversations: [mkConv({ id: 'b' })] },
    ];
    const newFlat = [mkConv({ id: 'b' }), mkConv({ id: 'a' })];

    const result = redistributeConversationsIntoGroups(prevGroups, newFlat);

    expect(result.find((g) => g.name === 'today')?.conversations.map((c) => c.id)).toEqual(['a']);
    expect(result.find((g) => g.name === 'older')?.conversations.map((c) => c.id)).toEqual(['b']);
  });

  it('defaults a never-seen-before id into the "today" bucket', () => {
    const prevGroups: Group[] = [
      { name: 'today', conversations: [] },
      { name: 'older', conversations: [] },
    ];
    const newFlat = [mkConv({ id: 'new-conv' })];

    const result = redistributeConversationsIntoGroups(prevGroups, newFlat);

    expect(result.find((g) => g.name === 'today')?.conversations.map((c) => c.id)).toEqual(['new-conv']);
    expect(result.find((g) => g.name === 'older')?.conversations).toEqual([]);
  });

  it('does NOT default an unknown id anywhere when there is no "today" group at all', () => {
    const prevGroups: Group[] = [{ name: 'older', conversations: [] }];
    const newFlat = [mkConv({ id: 'new-conv' })];

    const result = redistributeConversationsIntoGroups(prevGroups, newFlat);

    expect(result[0]?.conversations).toEqual([]);
  });

  it('preserves extra bucket fields (e.g. total/offset) untouched', () => {
    const prevGroups: Group[] = [{ name: 'today', conversations: [], total: 5, offset: 0 }];
    const result = redistributeConversationsIntoGroups(prevGroups, [] as Conversation[]);
    expect(result[0]).toMatchObject({ total: 5, offset: 0 });
  });
});

describe('sortConversations', () => {
  it('sorts by updatedAt (falling back to createdAt) descending', () => {
    const older = mkConv({ id: 'a', updatedAt: '2026-01-01T00:00:00Z' });
    const newer = mkConv({ id: 'b', updatedAt: '2026-02-01T00:00:00Z' });
    expect(sortConversations([older, newer]).map((c) => c.id)).toEqual(['b', 'a']);
  });

  it('falls back to createdAt when updatedAt is absent', () => {
    const older = mkConv({ id: 'a', createdAt: '2026-01-01T00:00:00Z' });
    const newer = mkConv({ id: 'b', createdAt: '2026-02-01T00:00:00Z' });
    expect(sortConversations([older, newer]).map((c) => c.id)).toEqual(['b', 'a']);
  });

  it('same id: a playback row always sorts before its live counterpart, regardless of date', () => {
    const live = mkConv({ id: 'x', isPlayback: false, updatedAt: '2026-02-01T00:00:00Z' });
    const playback = mkConv({ id: 'x', isPlayback: true, updatedAt: '2026-01-01T00:00:00Z' });
    expect(sortConversations([live, playback]).map((c) => c.isPlayback)).toEqual([true, false]);
  });

  it('different id: date wins first, isPlayback only breaks an exact date tie', () => {
    const samePlaybackFalse = mkConv({ id: 'a', isPlayback: false, updatedAt: '2026-01-01T00:00:00Z' });
    const samePlaybackTrue = mkConv({ id: 'b', isPlayback: true, updatedAt: '2026-01-01T00:00:00Z' });
    expect(sortConversations([samePlaybackFalse, samePlaybackTrue]).map((c) => c.id)).toEqual(['b', 'a']);

    const earlierPlayback = mkConv({ id: 'c', isPlayback: true, updatedAt: '2026-01-01T00:00:00Z' });
    const laterNonPlayback = mkConv({ id: 'd', isPlayback: false, updatedAt: '2026-02-01T00:00:00Z' });
    expect(sortConversations([earlierPlayback, laterNonPlayback]).map((c) => c.id)).toEqual(['d', 'c']);
  });

  it('does not throw and produces a stable order when both dates are missing on both sides', () => {
    const a = mkConv({ id: 'a' });
    const b = mkConv({ id: 'b' });
    expect(() => sortConversations([a, b])).not.toThrow();
    expect(sortConversations([a, b]).map((c) => c.id).sort()).toEqual(['a', 'b']);
  });
});

describe('pinnedFoldersFirst', () => {
  it('puts pinned folders first and keeps each side in its order', () => {
    const folders = [{ id: 'a' }, { id: 'b', isPinned: true }, { id: 'c' }, { id: 'd', isPinned: true }];
    expect(pinnedFoldersFirst(folders).map((folder) => folder.id)).toEqual(['b', 'd', 'a', 'c']);
  });
});

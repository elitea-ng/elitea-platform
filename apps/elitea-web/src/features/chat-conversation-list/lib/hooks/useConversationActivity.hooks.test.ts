import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';

import type { GroupedFoldersResponse } from '@/entities/folder';

import { applyConversationActivity, moveToToday } from './useConversationActivity.hooks';

const NOW = '2026-10-20T12:00:00.000Z';

function listing(): GroupedFoldersResponse {
  return {
    pinned: { conversations: [{ id: 'p1', name: 'pinned' }] },
    dateGroups: [
      { name: 'Today', conversations: [{ id: '1', name: 'one' }], total: 1, offset: 1 },
      { name: 'This month', conversations: [{ id: '9', name: 'nine' }, { id: '8', name: 'eight' }], total: 5, offset: 2 },
    ],
    folders: [{ id: 'f1', name: 'Specs', conversations: [{ id: '7', name: 'seven' }] }],
  } as unknown as GroupedFoldersResponse;
}

describe('moveToToday', () => {
  it('moves an ungrouped row to the top of Today, stamped now, and keeps the counters right', () => {
    const next = moveToToday(listing(), '8', NOW);
    expect(next.dateGroups.map((group) => [group.name, group.conversations.map((row) => row.id), group.total, group.offset])).toEqual([
      ['Today', ['8', '1'], 2, 2],
      ['This month', ['9'], 4, 1],
    ]);
    expect(next.dateGroups[0]?.conversations[0]?.updatedAt).toBe(NOW);
  });

  it('adds Today when the listing has none, and drops a group the move emptied', () => {
    const only: GroupedFoldersResponse = { ...listing(), dateGroups: [{ name: 'Older', conversations: [{ id: '3', name: 'three' }], total: 1, offset: 1 }] };
    expect(moveToToday(only, '3', NOW).dateGroups.map((group) => [group.name, group.conversations.map((row) => row.id)])).toEqual([['Today', ['3']]]);
  });

  it('leaves a folder row, a pinned row and the top row of Today where they are', () => {
    const start = listing();
    expect(moveToToday(start, '7', NOW)).toBe(start);
    expect(moveToToday(start, 'p1', NOW)).toBe(start);
    expect(moveToToday(start, '1', NOW)).toBe(start);
  });
});

describe('applyConversationActivity', () => {
  it('moves the row in every cached listing on a send, and reads the listing again when the turn settles', () => {
    const client = new QueryClient();
    client.setQueryData(['folder', 'list', '77', { sort_by: 'updated_at' }], listing());
    client.setQueryData(['folder', 'list', '77', { query: 'n' }], listing());
    applyConversationActivity(client, 'sent', '9');
    for (const [, data] of client.getQueriesData<GroupedFoldersResponse>({ queryKey: ['folder', 'list'] })) {
      expect(data?.dateGroups[0]?.conversations[0]?.id).toBe('9');
    }
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    applyConversationActivity(client, 'settled', '9');
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['folder', 'list'] });
  });
});

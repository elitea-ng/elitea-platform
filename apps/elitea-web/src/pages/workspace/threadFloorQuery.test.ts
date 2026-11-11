import { QueryClient } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { conversationApi } from '@/entities/conversation';

import { THREAD_FLOOR_STALE_MS, threadFloorQuery } from './useSendPrompt';

const detail = {
  meta: { floor: { policy: 'lead', lead_participant_id: 88 } },
  participants: [
    { id: 77, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } },
    { id: 88, entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer' }, entity_settings: { version_id: 10, executor: 'cloud' } },
  ],
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe('threadFloorQuery (#1928)', () => {
  it('reads a thread once per send window, and again when asked for a fresh read', async () => {
    const details = vi.spyOn(conversationApi, 'details').mockResolvedValue(detail as never);
    const client = new QueryClient();
    const query = threadFloorQuery(42, '77');

    const first = await client.fetchQuery(query);
    await client.fetchQuery(query);
    expect(details).toHaveBeenCalledTimes(1);
    expect(first.agents.map((agent) => agent.participantId)).toEqual([77, 88]);
    expect(first.meta).toEqual(detail.meta);
    expect(query.staleTime).toBe(THREAD_FLOOR_STALE_MS);

    await client.fetchQuery({ ...query, staleTime: 0 });
    expect(details).toHaveBeenCalledTimes(2);
  });

  it('keeps one copy per thread', async () => {
    const details = vi.spyOn(conversationApi, 'details').mockResolvedValue(detail as never);
    const client = new QueryClient();
    await client.fetchQuery(threadFloorQuery(42, '77'));
    await client.fetchQuery(threadFloorQuery(42, '78'));
    expect(details).toHaveBeenCalledTimes(2);
  });
});

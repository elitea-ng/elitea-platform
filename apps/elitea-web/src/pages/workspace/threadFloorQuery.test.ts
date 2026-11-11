import { QueryClient } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { conversationApi } from '@/entities/conversation';
import { getApplication } from '@/shared/api/generated/applications/applications';
import type { TurnStartRequest } from '@/shared/desktop/workspaceIpc';

import { rerouted, THREAD_FLOOR_STALE_MS, threadFloorQuery, withVersion } from './useSendPrompt';

vi.mock('@/shared/api/generated/applications/applications', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  getApplication: vi.fn(),
}));

const detail = {
  meta: { floor: { policy: 'lead', lead_participant_id: 88 } },
  participants: [
    { id: 77, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } },
    { id: 88, entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer' }, entity_settings: { version_id: 10, executor: 'cloud' } },
  ],
};

afterEach(() => {
  vi.restoreAllMocks();
  vi.mocked(getApplication).mockReset();
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

describe('withVersion (#1926)', () => {
  const unpinned = { addressed_to: [88], application_id: 6, version_id: 0, participant_id: 88 };

  it('runs an unpinned agent on its default version', async () => {
    vi.mocked(getApplication).mockResolvedValue({ versions: [{ id: 3, name: 'base' }] } as never);
    await expect(withVersion(42, unpinned)).resolves.toEqual({ ...unpinned, version_id: 3 });
  });

  it('keeps the address when the default version cannot be read, so the agent named still answers', async () => {
    vi.mocked(getApplication).mockRejectedValue(new Error('offline'));
    await expect(withVersion(42, unpinned)).resolves.toEqual({ addressed_to: [88] });
    vi.mocked(getApplication).mockResolvedValue({ versions: [] } as never);
    await expect(withVersion(42, unpinned)).resolves.toEqual({ addressed_to: [88] });
    const unaddressed = { application_id: 6, version_id: 0, participant_id: 88 };
    await expect(withVersion(42, unaddressed)).resolves.toEqual({});
  });

  it('leaves a pinned agent alone', async () => {
    await expect(withVersion(42, { ...unpinned, version_id: 10 })).resolves.toEqual({ ...unpinned, version_id: 10 });
    expect(getApplication).not.toHaveBeenCalled();
  });
});

describe('rerouted (#1931)', () => {
  const base: TurnStartRequest = { workspace_id: 'w', project_id: 42, conversation_id: '77', application_id: 5, version_id: 9, prompt: 'fix it', plan_mode: false, mentions: [], skills: [] };

  it('does not resend a start that falls back to the agent the refused one was on', async () => {
    // The thread now reads one agent: the retry would be the composer's agent again, with no participant named.
    vi.spyOn(conversationApi, 'details').mockResolvedValue({ meta: {}, participants: [detail.participants[0]] } as never);
    const refused = { ...base, participant_id: 77 };
    await expect(rerouted(new QueryClient(), refused, 42, '77', base)).resolves.toBeUndefined();
  });

  it('resends on the agent the thread now routes to', async () => {
    vi.spyOn(conversationApi, 'details').mockResolvedValue(detail as never);
    const refused = { ...base, participant_id: 77 };
    await expect(rerouted(new QueryClient(), refused, 42, '77', base)).resolves.toMatchObject({ application_id: 6, version_id: 10, participant_id: 88, cloud_agent: true });
  });
});

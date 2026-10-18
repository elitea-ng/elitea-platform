import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';

import { HANDOFF_EVENT, handoffInfoKey, parseHandoffEvent, useHandoffLiveEvents } from './useHandoffLiveEvents';
import { handoffStatusKey, handoffStatusManyKey } from './useHandoffStatus';

const globals = globalThis as unknown as Record<string, unknown>;
const UUID = '0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60';
let registry: TestEventSourceRegistry;

beforeEach(() => {
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

function setup(): { client: QueryClient; wrapper: (props: { readonly children: ReactNode }) => ReactNode } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  return { client, wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider> };
}

describe('useHandoffLiveEvents', () => {
  it('parses only a well-formed frame', () => {
    expect(parseHandoffEvent(JSON.stringify({ conversation_uuid: UUID.toUpperCase(), state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true }))).toEqual({
      conversation_uuid: UUID,
      state: 'CLOUD_RUNNING',
      epoch: 2,
      reclaim_pending: true,
    });
    for (const bad of ['not json', 'null', '{}', JSON.stringify({ conversation_uuid: UUID, state: 'ELSEWHERE', epoch: 2, reclaim_pending: false })]) {
      expect(parseHandoffEvent(bad)).toBeNull();
    }
  });

  it('updates the notice and refreshes the status reads from a frame, without a poll', async () => {
    const { client, wrapper } = setup();
    client.setQueryData(handoffInfoKey('7', UUID), { state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: false, device_name: 'laptop' });
    client.setQueryData([...handoffStatusKey(UUID), 7], { state: 'CLOUD_RUNNING' });
    client.setQueryData([...handoffStatusManyKey(), [UUID]], {});
    renderHook(() => useHandoffLiveEvents(7), { wrapper });
    await waitFor(() => {
      expect(registry.getOpen()).toHaveLength(1);
    });
    expect(registry.getSources()[0]?.url).toBe('/api/v2/events/prompt_lib/7');

    registry.emit(HANDOFF_EVENT, JSON.stringify({ conversation_uuid: UUID, state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true }));
    await waitFor(() => {
      expect(client.getQueryData(handoffInfoKey('7', UUID))).toMatchObject({ reclaim_pending: true, device_name: 'laptop' });
    });
    expect(client.getQueryState([...handoffStatusKey(UUID), 7])?.isInvalidated).toBe(true);
    expect(client.getQueryState([...handoffStatusManyKey(), [UUID]])?.isInvalidated).toBe(true);

    // A reordered, older frame never wins over the newer read.
    registry.emit(HANDOFF_EVENT, JSON.stringify({ conversation_uuid: UUID, state: 'CLOUD_IDLE', epoch: 1, reclaim_pending: false }));
    expect(client.getQueryData(handoffInfoKey('7', UUID))).toMatchObject({ state: 'CLOUD_RUNNING', reclaim_pending: true });
  });

  it('when the thread is local again, re-reads the thread details, not the other threads', async () => {
    const { client, wrapper } = setup();
    client.setQueryData(handoffInfoKey('7', UUID), { state: 'CLOUD_RUNNING', epoch: 1, reclaim_pending: false });
    const detailsKey = ['conversation', 'details', 7, 123] as const;
    const otherMessagesKey = ['conversation', 'messageList', 7, 'another-uuid', 0, 20] as const;
    client.setQueryData(detailsKey, { uuid: UUID });
    client.setQueryData(otherMessagesKey, []);
    renderHook(() => useHandoffLiveEvents(7), { wrapper });
    await waitFor(() => {
      expect(registry.getOpen()).toHaveLength(1);
    });

    registry.emit(HANDOFF_EVENT, JSON.stringify({ conversation_uuid: UUID, state: 'LOCAL', epoch: 2, reclaim_pending: false }));
    await waitFor(() => {
      expect(client.getQueryData(handoffInfoKey('7', UUID))).toMatchObject({ state: 'LOCAL', epoch: 2 });
    });
    expect(client.getQueryState(detailsKey)?.isInvalidated).toBe(true);
    // The other thread's message list is not this thread's move.
    expect(client.getQueryState(otherMessagesKey)?.isInvalidated).not.toBe(true);
  });

  it('opens no stream without a project', () => {
    const { wrapper } = setup();
    renderHook(() => useHandoffLiveEvents(null), { wrapper });
    expect(registry.getSources()).toHaveLength(0);
  });
});

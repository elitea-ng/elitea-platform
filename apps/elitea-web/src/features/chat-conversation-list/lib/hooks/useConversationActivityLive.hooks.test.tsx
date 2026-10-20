import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { applyActivityFrame, parseActivityFrame, resetConversationActivityOverlay, resolveRowActivity } from '@/shared/lib/conversationActivity';
import { server } from '@/test/setup';

import { useConversationActivityLiveEvents, useMarkConversationReadOnView } from './useConversationActivityLive.hooks';

const globals = globalThis as unknown as Record<string, unknown>;
let registry: TestEventSourceRegistry;
let reads: { readonly project: string; readonly conversation: string }[];

beforeEach(() => {
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: '1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  vi.spyOn(document, 'hasFocus').mockReturnValue(true);
  reads = [];
  server.use(
    http.put('*/elitea_core/conversation_read/prompt_lib/:projectId/:conversationId', ({ params }) => {
      reads.push({ project: String(params['projectId']), conversation: String(params['conversationId']) });
      return HttpResponse.json({ conversation_id: 12, read_at: new Date(Date.now() + 120_000).toISOString(), unread: false, last_read_message_id: 4 });
    }),
  );
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
  resetConversationActivityOverlay();
  vi.restoreAllMocks();
});

function wrapper(client: QueryClient) {
  return ({ children }: { readonly children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const newClient = (): QueryClient => new QueryClient({ defaultOptions: { queries: { retry: false } } });

describe('useConversationActivityLiveEvents', () => {
  it('applies the frames of the project stream to the rows', async () => {
    renderHook(() => useConversationActivityLiveEvents('7'), { wrapper: wrapper(newClient()) });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    expect(registry.getSources()[0]?.url).toBe('/api/v2/events/prompt_lib/7');
    const at = new Date().toISOString();
    registry.emit('conversation_activity', JSON.stringify({ conversation_id: 12, state: 'running', since: at, last_activity_at: at }));
    expect(resolveRowActivity({ id: '12' }).state).toBe('running');
    registry.emit('conversation_activity', 'not json');
    expect(resolveRowActivity({ id: '12' }).state).toBe('running');
  });

  it('reads the listing again when the window gets focus (the fallback for a missed frame)', async () => {
    const client = newClient();
    client.setQueryData(['folder', 'list', '7'], { pinned: { conversations: [] } });
    renderHook(() => useConversationActivityLiveEvents('7'), { wrapper: wrapper(client) });
    window.dispatchEvent(new Event('focus'));
    await waitFor(() => expect(client.getQueryState(['folder', 'list', '7'])?.isInvalidated).toBe(true));
  });

  it('opens nothing without a project', () => {
    renderHook(() => useConversationActivityLiveEvents(undefined), { wrapper: wrapper(newClient()) });
    expect(registry.getSources()).toHaveLength(0);
  });
});

describe('useMarkConversationReadOnView', () => {
  it('marks the open conversation read at once, and on the server', async () => {
    const at = new Date().toISOString();
    const row = { id: '12', activity: { state: 'idle' as const }, lastActivityAt: at, unread: true };
    renderHook(() => useMarkConversationReadOnView({ projectId: '7', conversationId: '12' }), { wrapper: wrapper(newClient()) });
    expect(resolveRowActivity(row).unread).toBe(false);
    await waitFor(() => expect(reads).toEqual([{ project: '7', conversation: '12' }]));
  });

  it('reads it again when a new answer arrives while it is open', async () => {
    renderHook(() => useMarkConversationReadOnView({ projectId: '7', conversationId: '12' }), { wrapper: wrapper(newClient()) });
    await waitFor(() => expect(reads).toHaveLength(1));
    const later = new Date(Date.now() + 600_000).toISOString();
    applyActivityFrame(parseActivityFrame(JSON.stringify({ conversation_id: 12, state: 'idle', last_activity_at: later, unread_hint: true }))!);
    await waitFor(() => expect(reads).toHaveLength(2));
  });

  it('does not mark a conversation the person cannot see', async () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);
    renderHook(() => useMarkConversationReadOnView({ projectId: '7', conversationId: '12' }), { wrapper: wrapper(newClient()) });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(reads).toHaveLength(0);
  });
});

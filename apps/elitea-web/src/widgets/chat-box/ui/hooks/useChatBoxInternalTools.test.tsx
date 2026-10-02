import { act, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useChatBoxInternalTools } from './useChatBoxInternalTools';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { http, HttpResponse } from 'msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

const edit = vi.fn<(body: Record<string, unknown>) => Promise<void>>();
async function mount(params: Parameters<typeof useChatBoxInternalTools>[0]) {
  const result = { current: undefined as ReturnType<typeof useChatBoxInternalTools> | undefined };
  function Probe() { result.current = useChatBoxInternalTools(params); return null; }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const route = createRootRoute({ component: Probe });
  const router = createRouter({ routeTree: route, history: createMemoryHistory({ initialEntries: ['/'] }) });
  const rendered = render(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>);
  await waitFor(() => expect(result.current?.internalToolsButtonTools.some(tool => tool.key === 'internal_mcp')).toBe(true));
  return { result: result as { current: ReturnType<typeof useChatBoxInternalTools> }, unmount: rendered.unmount };
}

const existing = { conversationId: 17, projectId: 7, conversationMeta: { other: true, internal_tools: [] }, isAgentsPage: false };
function deferred() {
  let resolve = () => undefined as void;
  let reject = (_error: Error) => undefined as void;
  const promise = new Promise<void>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  edit.mockReset().mockResolvedValue(undefined);
  server.use(http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true })));
  server.use(http.put('/api/v2/elitea_core/conversation/prompt_lib/7/17', async ({ request }) => {
    const body = await request.json() as Record<string, unknown>;
    try { await edit(body); } catch { return HttpResponse.json({ error: 'save failed' }, { status: 500 }); }
    return HttpResponse.json({ id: 17, ...body });
  }));
});
afterEach(resetGeneratedClient);
describe('chat internal tools selection', () => {
  it('keeps new-chat selection locally for creation without editing a missing conversation', async () => {
    const { result } = await mount({ ...existing, conversationId: undefined });
    act(() => result.current.handleInternalToolChange('internal_mcp', true));
    expect(result.current.internalToolsButtonTools.find(tool => tool.key === 'internal_mcp')?.enabled).toBe(true);
    expect(await result.current.getInternalToolsForSend()).toEqual(['internal_mcp']);
    expect(edit).not.toHaveBeenCalled();
  });

  it('waits for the durable update before permitting execution and restores selection on reload', async () => {
    const save = deferred();
    edit.mockReturnValue(save.promise);
    const { result, unmount } = await mount(existing);
    act(() => result.current.handleInternalToolChange('internal_mcp', true));
    const ready = vi.fn();
    const sending = result.current.getInternalToolsForSend().then(ready);
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(1));
    expect(edit).toHaveBeenCalledWith({ meta: { other: true, internal_tools: ['internal_mcp'] } });
    expect(ready).not.toHaveBeenCalled();
    await act(async () => { save.resolve(); await sending; });
    expect(ready).toHaveBeenCalledWith(['internal_mcp']);
    unmount();
    const persisted = { ...existing, conversationMeta: { other: true, internal_tools: ['internal_mcp'] } };
    const reload = await mount(persisted);
    expect(reload.result.current.internalToolsButtonTools.find(tool => tool.key === 'internal_mcp')?.enabled).toBe(true);
  });

  it('serializes rapid changes and deduplicates enabling the same tool', async () => {
    const save = deferred();
    edit.mockReturnValueOnce(save.promise);
    const { result } = await mount(existing);
    act(() => {
      result.current.handleInternalToolChange('internal_mcp', true);
      result.current.handleInternalToolChange('internal_mcp', true);
      result.current.handleInternalToolChange('internal_mcp', false);
    });
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(1));
    expect(edit).toHaveBeenCalledTimes(1);
    await act(async () => { save.resolve(); await result.current.getInternalToolsForSend(); });
    expect(edit.mock.calls.map(([input]) => (input['meta'] as { internal_tools: string[] }).internal_tools)).toEqual([['internal_mcp'], ['internal_mcp'], []]);
    expect(result.current.isUpdatingInternalToolsConfig).toBe(false);
  });

  it('rejects execution and rolls back display after persistence fails', async () => {
    edit.mockRejectedValue(new Error('save failed'));
    const { result } = await mount(existing);
    act(() => result.current.handleInternalToolChange('internal_mcp', true));
    await act(async () => { await expect(result.current.getInternalToolsForSend()).rejects.toThrow(); });
    expect(result.current.internalToolsButtonTools.find(tool => tool.key === 'internal_mcp')?.enabled).toBe(false);
  });
});

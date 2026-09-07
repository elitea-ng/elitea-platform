import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { useMcpLoadTools } from './useMcpLoadTools';
import type { EditToolDetail } from './toolkitFormTypes';

/**
 * `useGetRemoteMcpTools` posts with an `await_response` query parameter, so the
 * handler is registered on the path and msw matches the query itself.
 */
const PATH = '/api/v2/elitea_core/mcp_sync_tools/prompt_lib/:projectId';

function detail(overrides: Partial<EditToolDetail> = {}): EditToolDetail {
  return { type: 'mcp', settings: {}, ...overrides };
}

/** `useGetRemoteMcpTools` reads the socket id, so the hook needs the socket context. */
function wrapper({ children }: { children: React.ReactNode }) {
  return <SocketClientContext.Provider value={createTestSocketClient()}>{children}</SocketClientContext.Provider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
  localStorage.clear();
});

describe('useMcpLoadTools', () => {
  it('contributes nothing for a toolkit that is not MCP-shaped', () => {
    const { result } = renderHook(
      () => useMcpLoadTools({ projectId: '1', editToolDetail: detail({ type: 'github' }), onChangeToolDetail: vi.fn() }),
      { wrapper },
    );
    expect(result.current).toBeUndefined();
  });

  it('contributes nothing when there is no toolkit at all', () => {
    const { result } = renderHook(() => useMcpLoadTools({ projectId: '1', editToolDetail: null, onChangeToolDetail: vi.fn() }), { wrapper });
    expect(result.current).toBeUndefined();
  });

  /**
   * The gate that makes the action clickable. A generic Remote MCP toolkit has
   * nothing to dial until the user types a URL; a pre-built one never does,
   * because the catalogue row supplies its URL server-side.
   */
  it.each([
    { name: 'a Remote MCP with no URL cannot load', toolkitType: 'mcp', settings: {}, can: false },
    { name: 'a Remote MCP with a blank URL cannot load', toolkitType: 'mcp', settings: { url: '   ' }, can: false },
    { name: 'a Remote MCP with a URL can load', toolkitType: 'mcp', settings: { url: 'https://mcp-mock:8443/mcp' }, can: true },
    { name: 'a pre-built MCP can load with no URL of its own', toolkitType: 'mcp_context7', settings: {}, can: true },
  ])('$name', ({ toolkitType, settings, can }) => {
    const { result } = renderHook(
      () => useMcpLoadTools({ projectId: '1', editToolDetail: detail({ type: toolkitType, settings }), onChangeToolDetail: vi.fn() }),
      { wrapper },
    );
    expect(result.current?.canLoadTools).toBe(can);
  });

  it('cannot load before a project is resolved', () => {
    const { result } = renderHook(
      () =>
        useMcpLoadTools({
          projectId: undefined,
          editToolDetail: detail({ settings: { url: 'https://mcp-mock:8443/mcp' } }),
          onChangeToolDetail: vi.fn(),
        }),
      { wrapper },
    );
    expect(result.current?.canLoadTools).toBe(false);
  });

  /**
   * The round trip the whole slot exists for: the discovered names land in
   * `settings.available_mcp_tools`, which is the exact key
   * `ToolBase.render.tsx`'s `resolveAvailableTools` reads for a type that
   * declares no `args_schemas`. Writing them anywhere else leaves the chip
   * picker empty with the request having succeeded.
   */
  it('writes the discovered tool names into settings.available_mcp_tools', async () => {
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(PATH, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ success: true, tools: [{ name: 'echo' }, { name: 'ping' }, { name: '' }] });
      }),
    );
    const onChangeToolDetail = vi.fn();
    const editToolDetail = detail({
      settings: { url: 'https://mcp-mock:8443/mcp', timeout: 120, headers: { Authorization: 'Bearer t' } },
    });

    const { result } = renderHook(() => useMcpLoadTools({ projectId: '1', editToolDetail, onChangeToolDetail }), { wrapper });
    act(() => result.current?.onLoadTools());

    await waitFor(() => expect(onChangeToolDetail).toHaveBeenCalledTimes(1));
    const updater = onChangeToolDetail.mock.calls[0]?.[0] as (previous: EditToolDetail | null) => EditToolDetail | null;
    expect(updater(editToolDetail)?.settings).toMatchObject({
      url: 'https://mcp-mock:8443/mcp',
      // A nameless tool is dropped: a blank chip is unselectable.
      available_mcp_tools: ['echo', 'ping'],
    });
    expect(sent).toMatchObject({ url: 'https://mcp-mock:8443/mcp', timeout: 120, headers: { Authorization: 'Bearer t' } });
  });

  /**
   * A generic Remote MCP is resolved by dynamic discovery from its own stored
   * `settings.url` at run time — the SDK's `_mcp_tools` skips `type == 'mcp'`
   * outright — so the discovery must NOT ask elitea-main to register a server
   * under that name. A pre-built type must, because that is the name the SDK
   * looks up. This asserts both directions of that asymmetry on the wire.
   */
  it('registers a pre-built server by type and leaves a generic Remote MCP unregistered', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post(PATH, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ success: true, tools: [] });
      }),
    );

    const remote = renderHook(
      () =>
        useMcpLoadTools({
          projectId: '1',
          editToolDetail: detail({ type: 'mcp', settings: { url: 'https://mcp-mock:8443/mcp' } }),
          onChangeToolDetail: vi.fn(),
        }),
      { wrapper },
    );
    act(() => remote.result.current?.onLoadTools());
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]?.['toolkit_type']).toBeUndefined();

    const prebuilt = renderHook(
      () => useMcpLoadTools({ projectId: '1', editToolDetail: detail({ type: 'mcp_context7', settings: {} }), onChangeToolDetail: vi.fn() }),
      { wrapper },
    );
    act(() => prebuilt.result.current?.onLoadTools());
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]?.['toolkit_type']).toBe('mcp_context7');
  });

  it('reads a timeout the form stored as a string', async () => {
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(PATH, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ success: true, tools: [] });
      }),
    );
    const { result } = renderHook(
      () =>
        useMcpLoadTools({
          projectId: '1',
          editToolDetail: detail({ settings: { url: 'https://mcp-mock:8443/mcp', timeout: '90' } }),
          onChangeToolDetail: vi.fn(),
        }),
      { wrapper },
    );
    act(() => result.current?.onLoadTools());

    await waitFor(() => expect(sent).toMatchObject({ timeout: 90 }));
  });

  /**
   * A failed discovery answers HTTP 200 with `success: false`. Writing an empty
   * tool list here would report a dead server as one that publishes no tools.
   */
  it('reports a failed discovery and writes no tools', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ success: false, error: 'MCP tool discovery failed' })));
    const onChangeToolDetail = vi.fn();

    const { result } = renderHook(
      () =>
        useMcpLoadTools({
          projectId: '1',
          editToolDetail: detail({ settings: { url: 'https://dead.example/mcp' } }),
          onChangeToolDetail,
        }),
      { wrapper },
    );
    act(() => result.current?.onLoadTools());

    await waitFor(() => expect(result.current?.isLoadingTools).toBe(false));
    expect(onChangeToolDetail).not.toHaveBeenCalled();
  });

  it('reports it is loading while the request is in flight', async () => {
    let release: (() => void) | undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.post(PATH, async () => {
        await held;
        return HttpResponse.json({ success: true, tools: [] });
      }),
    );
    const { result } = renderHook(
      () =>
        useMcpLoadTools({
          projectId: '1',
          editToolDetail: detail({ settings: { url: 'https://mcp-mock:8443/mcp' } }),
          onChangeToolDetail: vi.fn(),
        }),
      { wrapper },
    );

    act(() => result.current?.onLoadTools());
    await waitFor(() => expect(result.current?.isLoadingTools).toBe(true));

    act(() => release?.());
    await waitFor(() => expect(result.current?.isLoadingTools).toBe(false));
  });

  it('carries the real MCP authorization modal in its slot', () => {
    const { result } = renderHook(
      () =>
        useMcpLoadTools({
          projectId: '1',
          editToolDetail: detail({ settings: { url: 'https://mcp-mock:8443/mcp' } }),
          onChangeToolDetail: vi.fn(),
        }),
      { wrapper },
    );
    // The slot is what `ToolActionsSelector` renders. Before this file existed
    // it was never supplied at all, so a server that answers 401 could not be
    // authorized from the toolkit form.
    expect(result.current?.mcpAuthModal).not.toBeNull();
  });
});

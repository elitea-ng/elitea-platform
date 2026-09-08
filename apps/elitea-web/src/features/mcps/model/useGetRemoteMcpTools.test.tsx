import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import type { TestSocketClient } from '@/shared/api/socket/testing';

import { server } from '../../../test/setup';
import { getAccessToken, setAccessToken } from '../lib/storage';

import { useGetRemoteMcpTools } from './useGetRemoteMcpTools';

function withSocket(client: TestSocketClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <SocketClientContext.Provider value={client}>{children}</SocketClientContext.Provider>;
  };
}

afterEach(() => {
  window.sessionStorage.clear();
  resetGeneratedClient();
});

describe('useGetRemoteMcpTools', () => {
  it('rejects immediately (no request) when a remote MCP has no url', () => {
    const client = createTestSocketClient();
    const onError = vi.fn();
    const { result } = renderHook(() => useGetRemoteMcpTools({ values: { type: 'mcp', settings: {} }, onError }), {
      wrapper: withSocket(client),
    });

    act(() => result.current.fetchTools());
    expect(onError).toHaveBeenCalledWith('MCP server URL is required');
  });

  it('fetches tools and reports them via onToolsFetched, including sid/awaitResponse plumbing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    const onToolsFetched = vi.fn();
    const onSuccess = vi.fn();
    let capturedBody: unknown;
    let capturedUrl = '';
    server.use(
      http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', async ({ request }) => {
        capturedUrl = request.url;
        capturedBody = await request.json();
        return HttpResponse.json({ success: true, tools: [{ name: 'list_files' }, { name: 'read_file' }] });
      }),
    );

    const { result } = renderHook(
      () => useGetRemoteMcpTools({ values: { type: 'mcp', settings: { url: 'https://mcp.example.com' } }, projectId: 5, onToolsFetched, onSuccess }),
      { wrapper: withSocket(client) },
    );

    act(() => result.current.fetchTools());
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(onToolsFetched).toHaveBeenCalledWith([{ name: 'list_files' }, { name: 'read_file' }], undefined);
    expect(onSuccess).toHaveBeenCalledWith(2);
    expect(capturedUrl).toContain('await_response=true');
    expect(capturedBody).toMatchObject({ url: 'https://mcp.example.com', sid: 'test-socket-client' });
  });

  it('opens the OAuth modal when the sync response reports requires_authorization', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    server.use(
      http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', () =>
        HttpResponse.json({ requires_authorization: true, response_metadata: { resource_metadata: { authorization_servers: ['https://as.example.com'] } } }),
      ),
    );

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, values: { type: 'mcp', settings: { url: 'https://needs-auth.example.com' } } }), {
      wrapper: withSocket(client),
    });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(result.current.getModalProps().open).toBe(true));
  });

  it('surfaces a friendlier message for a 401-shaped explicit failure', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    const onError = vi.fn();
    server.use(http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', () => HttpResponse.json({ success: false, error: '401 Unauthorized' })));

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, values: { type: 'mcp', settings: { url: 'https://denied.example.com' } }, onError }), {
      wrapper: withSocket(client),
    });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(onError).toHaveBeenCalledWith('Authorization failed. Please try logging in again.');
  });

  it('surfaces a network/transport failure via onError rather than throwing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    const onError = vi.fn();
    server.use(http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', () => HttpResponse.json({ error: 'server exploded' }, { status: 500 })));

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, values: { type: 'mcp', settings: { url: 'https://five-hundred.example.com' } }, onError }), {
      wrapper: withSocket(client),
    });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(onError).toHaveBeenCalled());
  });

  it('works for a pre-built MCP keyed by toolkitType, without requiring a url', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    const onToolsFetched = vi.fn();
    let capturedBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ success: true, tools: [{ name: 'search_repos' }] });
      }),
    );

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, toolkitType: 'mcp_github', onToolsFetched }), { wrapper: withSocket(client) });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(onToolsFetched).toHaveBeenCalled());
    expect(capturedBody).toMatchObject({ toolkit_type: 'mcp_github' });
  });

  // Regression test for a warning: a successful `mcp_sync_tools` response
  // used to skip `setConnectionVerified` entirely, so a header-based-auth
  // (non-OAuth) remote MCP's own login/connected UI (`useMcpTokenChange`'s
  // `isLoggedIn`, keyed off `getAccessToken`) never flipped even though
  // tools were fetched successfully.
  it('marks a header-auth remote MCP connection verified after a successful tools fetch', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    server.use(
      http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', () => HttpResponse.json({ success: true, tools: [{ name: 'list_files' }] })),
    );

    expect(getAccessToken('https://verify-me.example.com')).toBeNull();

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, values: { type: 'mcp', settings: { url: 'https://verify-me.example.com' } } }), {
      wrapper: withSocket(client),
    });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(getAccessToken('https://verify-me.example.com')).toBeTruthy();
  });

  // Same regression, for the pre-built-MCP (toolkitType-keyed) path.
  it('marks a pre-built MCP connection verified after a successful tools fetch', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const client = createTestSocketClient();
    server.use(http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/1', () => HttpResponse.json({ success: true, tools: [{ name: 'search_repos' }] })));

    expect(getAccessToken(undefined, 'mcp_github')).toBeNull();

    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 1, toolkitType: 'mcp_github' }), { wrapper: withSocket(client) });

    act(() => result.current.fetchTools());
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(getAccessToken(undefined, 'mcp_github')).toBeTruthy();
  });

  it('requires an explicit project instead of sending discovery to project one', () => {
    const onError = vi.fn();
    const { result } = renderHook(() => useGetRemoteMcpTools({ toolkitType: 'mcp_github', onError }), { wrapper: withSocket(createTestSocketClient()) });
    act(() => result.current.fetchTools());
    expect(onError).toHaveBeenCalledWith('A project is required to load MCP tools');
  });

  it('awaits refresh and sends only this project/resource access token, without grant secrets', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const url = 'https://resource.example.com/mcp';
    setAccessToken(url, 'expired', -1, 'session', undefined, 'refresh-secret', { project_id: '5', token_endpoint: 'https://issuer.example.com/token', client_id: 'client', client_secret: 'client-secret' });
    setAccessToken('https://other-resource.example.com/mcp', 'other-resource', 3600, undefined, undefined, undefined, { project_id: '5' });
    setAccessToken('https://other-project.example.com/mcp', 'other-project', 3600, undefined, undefined, undefined, { project_id: '6' });
    let body: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/5', () => HttpResponse.json({ access_token: 'fresh', expires_in: 3600 })),
      http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ success: true, tools: [] });
      }),
    );
    const onToolsFetched = vi.fn();
    const { result } = renderHook(() => useGetRemoteMcpTools({ values: { type: 'mcp', settings: { url } }, projectId: 5, onToolsFetched }), { wrapper: withSocket(createTestSocketClient()) });
    act(() => result.current.fetchTools());
    await waitFor(() => expect(onToolsFetched).toHaveBeenCalled());
    expect(body).toMatchObject({ mcp_tokens: { [url]: { access_token: 'fresh', session_id: 'session' } } });
    for (const secret of ['expired', 'other-resource', 'other-project', 'refresh-secret', 'client-secret']) expect(JSON.stringify(body)).not.toContain(secret);
  });

  it('does not apply a late discovery response to a different resource form', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let release!: () => void;
    let started = false;
    const pending = new Promise<void>((resolve) => { release = resolve; });
    server.use(http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', async () => {
      started = true;
      await pending;
      return HttpResponse.json({ success: true, tools: [{ name: 'old_resource_tool' }] });
    }));
    const onToolsFetched = vi.fn();
    const { result, rerender } = renderHook(({ url }) => useGetRemoteMcpTools({ values: { type: 'mcp', settings: { url } }, projectId: 5, onToolsFetched }), {
      initialProps: { url: 'https://first.example.com/mcp' }, wrapper: withSocket(createTestSocketClient()),
    });
    act(() => result.current.fetchTools());
    await waitFor(() => expect(started).toBe(true));
    rerender({ url: 'https://second.example.com/mcp' });
    await act(async () => { release(); await pending; });
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(onToolsFetched).not.toHaveBeenCalled();
  });

  it('retries discovery once when browser authorization succeeds', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let requests = 0;
    server.use(http.post('*/api/v2/elitea_core/mcp_sync_tools/prompt_lib/5', () => {
      requests++;
      return HttpResponse.json(requests === 1
        ? { requires_authorization: true, response_metadata: { server_url: 'https://resource.example.com/mcp', resource_metadata: { authorization_servers: ['https://issuer.example.com'] } } }
        : { success: true, tools: [{ name: 'echo_marker' }] });
    }));
    const onToolsFetched = vi.fn();
    const { result } = renderHook(() => useGetRemoteMcpTools({ projectId: 5, values: { type: 'mcp', settings: { url: 'https://resource.example.com/mcp' } }, onToolsFetched }), { wrapper: withSocket(createTestSocketClient()) });
    act(() => result.current.fetchTools());
    await waitFor(() => expect(result.current.getModalProps().open).toBe(true));
    act(() => result.current.getModalProps().onClose(true));
    await waitFor(() => expect(onToolsFetched).toHaveBeenCalledOnce());
    expect(requests).toBe(2);
  });
});

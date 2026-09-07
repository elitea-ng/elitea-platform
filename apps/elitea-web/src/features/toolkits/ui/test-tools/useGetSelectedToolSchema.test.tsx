import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../__tests__/testUtils';

import type { UseGetSelectedToolSchemaParams } from './useGetSelectedToolSchema';
import { useGetSelectedToolSchema } from './useGetSelectedToolSchema';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderSelectedToolSchema(params: UseGetSelectedToolSchemaParams): { readonly box: { current: ReturnType<typeof useGetSelectedToolSchema> | undefined } } {
  const box: { current: ReturnType<typeof useGetSelectedToolSchema> | undefined } = { current: undefined };

  function ProbeComponent() {
    box.current = useGetSelectedToolSchema(params);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });

  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  return { box };
}

describe('useGetSelectedToolSchema', () => {
  it('returns null when no tool is selected', async () => {
    let requestCount = 0;
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => {
        requestCount += 1;
        return HttpResponse.json({ github: {} });
      }),
    );
    const { box } = renderSelectedToolSchema({ toolkitType: 'github', toolOptionType: null, availableMcpTools: undefined });
    await waitFor(() => expect(requestCount).toBe(1));
    expect(box.current?.toolSchema).toBeNull();
  });

  it('returns null when the toolkit type has no schema loaded', async () => {
    let requestCount = 0;
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => {
        requestCount += 1;
        return HttpResponse.json({});
      }),
    );
    const { box } = renderSelectedToolSchema({ toolkitType: 'github', toolOptionType: 'list_issues', availableMcpTools: undefined });
    await waitFor(() => expect(requestCount).toBe(1));
    expect(box.current?.toolSchema).toBeNull();
  });

  it('resolves the static args_schemas entry for the selected tool', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: {
            properties: {
              selected_tools: {
                args_schemas: {
                  list_issues: { properties: { repo: { type: 'string' } }, required: ['repo'] },
                },
              },
            },
          },
        }),
      ),
    );

    const { box } = renderSelectedToolSchema({ toolkitType: 'github', toolOptionType: 'list_issues', availableMcpTools: undefined });
    await waitFor(() => expect(box.current?.toolSchema).toEqual({ properties: { repo: { type: 'string' } }, required: ['repo'] }));
  });

  it('falls back to a pre-loaded MCP args_schema when there is no static entry', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ mcp_github: {} })));

    const { box } = renderSelectedToolSchema({
      toolkitType: 'mcp_github',
      toolOptionType: 'search_repos',
      availableMcpTools: [{ value: 'search_repos', args_schema: { properties: { q: { type: 'string' } } } }],
    });

    await waitFor(() => expect(box.current?.toolSchema).toEqual({ properties: { q: { type: 'string' } } }));
  });

  it('normalises an MCP schema wrapped in inputSchema into the flat JSON-Schema-like shape', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ mcp_github: {} })));

    const { box } = renderSelectedToolSchema({
      toolkitType: 'mcp_github',
      toolOptionType: 'search_repos',
      availableMcpTools: [
        {
          value: 'search_repos',
          args_schema: {
            inputSchema: { properties: { q: { type: 'string' } }, required: ['q'] },
            title: 'Search repos',
            description: 'Search GitHub repositories',
          },
        },
      ],
    });

    await waitFor(() =>
      expect(box.current?.toolSchema).toEqual({
        properties: { q: { type: 'string' } },
        required: ['q'],
        title: 'Search repos',
        description: 'Search GitHub repositories',
        type: 'object',
      }),
    );
  });

  /**
   * #440 corrected the reason, not the outcome. The
   * `toolkit_available_tools` route exists and `TestToolSettings.tsx` reads
   * it, but its rows carry `id`/`name`/`type`/`description` only — no
   * argument schema — so this hook still has nothing to resolve a dynamic
   * tool's argument form from. The assertion below therefore stands; the
   * title no longer claims a missing endpoint.
   */
  it('returns null for a dynamic (non-static, non-MCP) tool — the tool catalogue carries no argument schema', async () => {
    let requestCount = 0;
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => {
        requestCount += 1;
        return HttpResponse.json({ openapi_tool: { properties: { selected_tools: { items: { enum: ['dynamic_op'] } } } } });
      }),
    );

    const { box } = renderSelectedToolSchema({ toolkitType: 'openapi_tool', toolOptionType: 'dynamic_op', availableMcpTools: undefined });
    await waitFor(() => expect(requestCount).toBe(1));
    expect(box.current?.toolSchema).toBeNull();
    // The read SUCCEEDED and carried no schema. That is not a failure.
    expect(box.current?.isError).toBe(false);
  });

  /**
   * #440. `null` used to be the only outcome this hook could report, so an
   * argument form with no fields meant either "this tool takes no arguments"
   * or "the read that carries the arguments was lost". The pair below keeps
   * the two apart: same `toolSchema === null`, different `isError`.
   */
  it('reports a failed schema read as its own signal, beside the same null schema', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ error: 'schemas unavailable' }, { status: 500 })));

    const { box } = renderSelectedToolSchema({ toolkitType: 'github', toolOptionType: 'list_issues', availableMcpTools: undefined });

    await waitFor(() => expect(box.current?.isError).toBe(true));
    expect(box.current?.toolSchema).toBeNull();
  });

  it('reads the schemas again when the caller retries', async () => {
    let requestCount = 0;
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => {
        requestCount += 1;
        return HttpResponse.json({ error: 'schemas unavailable' }, { status: 500 });
      }),
    );

    const { box } = renderSelectedToolSchema({ toolkitType: 'github', toolOptionType: 'list_issues', availableMcpTools: undefined });

    await waitFor(() => expect(box.current?.isError).toBe(true));
    expect(requestCount).toBe(1);
    box.current?.refetch();
    await waitFor(() => expect(requestCount).toBe(2));
  });
});

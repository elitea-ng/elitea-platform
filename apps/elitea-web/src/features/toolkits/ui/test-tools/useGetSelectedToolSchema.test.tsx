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

  it('loads the saved instance argument schema for an OpenAPI operation', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ openapi: {} })),
      http.get('/api/v2/elitea_core/toolkit_available_tools/prompt_lib/proj-1/31', () => HttpResponse.json({
        tools: [{ name: 'echo_marker' }],
        args_schemas: { echo_marker: { type: 'object', properties: { marker: { type: 'string' } }, required: ['marker'] } },
      })),
    );
    const { box } = renderSelectedToolSchema({ toolkitType: 'openapi', toolkitId: '31', toolOptionType: 'echo_marker', availableMcpTools: undefined });
    await waitFor(() => expect(box.current?.toolSchema?.required).toEqual(['marker']));
    expect(box.current?.toolSchema?.properties).toEqual({ marker: { type: 'string' } });
    expect(box.current?.isError).toBe(false);
  });

  it('reports a failed instance discovery and retries that read', async () => {
    let reads = 0;
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ openapi: {} })),
      http.get('/api/v2/elitea_core/toolkit_available_tools/prompt_lib/proj-1/31', () => {
        reads += 1;
        return reads === 1 ? HttpResponse.json({}, { status: 503 }) : HttpResponse.json({
          tools: [{ name: 'ping' }], args_schemas: { ping: { type: 'object', properties: {} } },
        });
      }),
    );
    const { box } = renderSelectedToolSchema({ toolkitType: 'openapi', toolkitId: '31', toolOptionType: 'ping', availableMcpTools: undefined });
    await waitFor(() => expect(box.current?.isError).toBe(true));
    expect(box.current?.toolSchema).toBeNull();
    box.current?.refetch();
    await waitFor(() => expect(box.current?.toolSchema?.properties).toEqual({}));
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

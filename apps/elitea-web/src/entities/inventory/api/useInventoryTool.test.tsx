/**
 * One Inventory read, as a react-query query.
 *
 * Three rules live here and each one fails without a symptom. Every read tool
 * answers MARKDOWN unless `output_format: 'json'` is sent, so a panel that
 * forgets the argument gets a formatted report where it expected rows, renders
 * nothing, and reports no error. The query key carries the ARGUMENTS, so two
 * panels reading `list_entities_by_type` for two different types are two reads
 * — a key naming only the tool serves the second panel the first one's rows.
 * And `retry: false` keeps a refusal a two-second answer instead of a
 * ten-second one that says the same thing three times.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { INVENTORY_FAMILY, type InventoryTarget } from './inventoryToolApi';
import { inventoryToolkitToolsKey, useInventoryTool } from './useInventoryTool';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = {
  projectId: '7',
  toolkitId: '42',
  settings: { bucket: 'graphs' },
};

interface Invoke {
  readonly tool: string;
  readonly parameters: Record<string, unknown>;
}

/** Accept every invoke, and answer every poll with one completed document. */
function serveTool(document: unknown): Invoke[] {
  const invokes: Invoke[] = [];
  server.use(
    http.post(INVOKE_ROUTE, async ({ params, request }) => {
      const body = (await request.json()) as { parameters: Record<string, unknown> };
      invokes.push({ tool: String(params['tool']), parameters: body.parameters });
      return HttpResponse.json({ invocation_id: `inv-${invokes.length}`, status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, () =>
      HttpResponse.json({
        status: 'Completed',
        result: JSON.stringify([
          { object_type: 'message', result_target: 'response', data: JSON.stringify(document) },
        ]),
      }),
    ),
  );
  return invokes;
}

function harness(): {
  readonly client: QueryClient;
  readonly wrapper: (props: { children: ReactNode }) => React.JSX.Element;
} {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return {
    client,
    wrapper: function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    },
  };
}

function wrapper(): (props: { children: ReactNode }) => React.JSX.Element {
  return harness().wrapper;
}

/** The keys the cache actually holds — the key builder is module-private. */
function keysIn(client: QueryClient): readonly (readonly unknown[])[] {
  return client
    .getQueryCache()
    .getAll()
    .map((query) => query.queryKey);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('the query key one read is cached under', () => {
  it('names the toolkit, the family, the tool AND the arguments', async () => {
    const invokes = serveTool({ entities: [] });
    const { client, wrapper: Wrapper } = harness();
    renderHook(
      () =>
        useInventoryTool(TARGET, INVENTORY_FAMILY, 'list_entities_by_type', {
          entity_type: 'class',
        }),
      { wrapper: Wrapper },
    );
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });

    expect(keysIn(client)).toEqual([
      [
        'inventory',
        'tool',
        '7',
        '42',
        'inventory',
        'list_entities_by_type',
        '{"output_format":"json","entity_type":"class"}',
      ],
    ]);
  });

  it('is PREFIXED by the key an ingestion invalidates', () => {
    // An ingestion changes the graph, so the sources, the entity lists and the
    // statistics are stale together — they share this prefix and nothing else.
    expect(inventoryToolkitToolsKey(TARGET)).toEqual(['inventory', 'tool', '7', '42']);
  });
});

describe('useInventoryTool', () => {
  it('sends output_format:json on every read, and answers the document', async () => {
    const invokes = serveTool({ node_count: 6 });
    const { result } = renderHook(
      () => useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}),
      { wrapper: wrapper() },
    );

    await waitFor(() => {
      expect(result.current.data?.document).toEqual({ node_count: 6 });
    });
    expect(invokes).toEqual([{ tool: 'get_stats', parameters: { output_format: 'json' } }]);
  });

  it('keeps the caller own arguments beside it', async () => {
    const invokes = serveTool({ results: [] });
    renderHook(
      () =>
        useInventoryTool(TARGET, INVENTORY_FAMILY, 'search_graph', { query: 'checkout', top_k: 200 }),
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });
    expect(invokes[0]?.parameters).toEqual({
      output_format: 'json',
      query: 'checkout',
      top_k: 200,
    });
  });

  it('runs two invocations for two different argument sets', async () => {
    // One key per (tool, arguments) pair. Sharing a key would serve the second
    // panel the first one rows.
    const invokes = serveTool({ entities: [] });
    renderHook(
      () => {
        useInventoryTool(TARGET, INVENTORY_FAMILY, 'list_entities_by_type', { entity_type: 'class' });
        useInventoryTool(TARGET, INVENTORY_FAMILY, 'list_entities_by_type', { entity_type: 'function' });
      },
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(invokes).toHaveLength(2);
    });
  });

  it('does not invoke while the panel is off screen', async () => {
    // An invocation is a unit of work on the engine, not a cached GET; a hidden
    // tab that polls is real load for nothing.
    const invokes = serveTool({});
    const { result } = renderHook(
      () => useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}, { enabled: false }),
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(result.current.fetchStatus).toBe('idle');
    });
    expect(invokes).toEqual([]);
  });

  it('does not invoke before a toolkit has been chosen', async () => {
    const invokes = serveTool({});
    const { result } = renderHook(
      () =>
        useInventoryTool({ ...TARGET, toolkitId: '' }, INVENTORY_FAMILY, 'get_stats', {}),
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(result.current.fetchStatus).toBe('idle');
    });
    expect(invokes).toEqual([]);
  });

  it('reports a refusal ONCE, without retrying a terminal answer', async () => {
    let invokeCount = 0;
    server.use(
      http.post(INVOKE_ROUTE, () => {
        invokeCount += 1;
        return HttpResponse.json({ invocation_id: 'inv-1', status: 'Started' });
      }),
      http.get(INVOCATION_ROUTE, () =>
        HttpResponse.json({
          status: 'Error',
          result: JSON.stringify([
            { object_type: 'message', result_target: 'response', data: 'No such entity.' },
          ]),
          error_category: 'resource_not_found',
        }),
      ),
    );

    const { result } = renderHook(
      () => useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_entity', { entity_id: 'nope' }),
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(result.current.error?.message).toBe('No such entity.');
    });
    expect(invokeCount).toBe(1);
  });
});

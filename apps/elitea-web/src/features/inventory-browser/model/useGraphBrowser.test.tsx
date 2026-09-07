/**
 * Which of four tools answers a filter, and with which arguments.
 *
 * `toolForFilter` is the whole browser in one function, and every branch of it
 * fails as an EMPTY LIST rather than as an error. Send a type filter to
 * `search_graph` with no `entity_type` and the provider answers the whole graph;
 * send an empty query to nothing at all and the screen opens on a blank list
 * with no way to see what is there. Worse, filtering a fetched page in the
 * browser instead of naming the argument narrows only the first `top_k` rows
 * and reports the result as the whole graph — "there are three classes" on a
 * graph with three hundred.
 *
 * The empty-string guards matter too: the provider's merge takes a tool
 * argument over a configured one only when it is TRUTHY, so an empty
 * `source_toolkit` sent anyway invites a reader to believe it clears a
 * configured value.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import type { InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { EMPTY_GRAPH_FILTER, toolForFilter, useGraphBrowser } from './useGraphBrowser';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = { projectId: '7', toolkitId: '42', settings: { bucket: 'graphs' } };

const HIT = {
  id: 'code:checkout-service',
  name: 'CheckoutService',
  type: 'class',
  layer: 'application',
  source_toolkit: 'code',
  file_path: 'src/checkout/service.py',
};

interface Invoke {
  readonly tool: string;
  readonly parameters: Record<string, unknown>;
}

function serveGraph(document: unknown, failWith?: string): Invoke[] {
  const invokes: Invoke[] = [];
  server.use(
    http.post(INVOKE_ROUTE, async ({ params, request }) => {
      const body = (await request.json()) as { parameters: Record<string, unknown> };
      invokes.push({ tool: String(params['tool']), parameters: body.parameters });
      return HttpResponse.json({ invocation_id: 'inv-1', status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, () => {
      const data = failWith ?? JSON.stringify(document);
      const result = JSON.stringify([
        { object_type: 'message', result_target: 'response', data },
      ]);
      return HttpResponse.json(
        failWith === undefined ? { status: 'Completed', result } : { status: 'Error', result },
      );
    }),
  );
  return invokes;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('toolForFilter', () => {
  it('reads an empty filter as EVERYTHING, which the provider matcher agrees with', () => {
    // A screen that refused to read without a filter would open on nothing.
    expect(toolForFilter(EMPTY_GRAPH_FILTER)).toEqual({
      tool: 'search_graph',
      params: { query: '', top_k: 200 },
    });
  });

  it('sends a query to search_graph, trimmed, with every facet beside it', () => {
    expect(
      toolForFilter({
        query: '  checkout  ',
        entityType: 'class',
        layer: 'application',
        sourceToolkit: 'code',
      }),
    ).toEqual({
      tool: 'search_graph',
      params: {
        query: 'checkout',
        top_k: 200,
        entity_type: 'class',
        layer: 'application',
        source_toolkit: 'code',
      },
    });
  });

  it('omits an unset facet rather than sending an empty string', () => {
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, query: 'checkout' }).params).toEqual({
      query: 'checkout',
      top_k: 200,
    });
  });

  it('reads whitespace as no query at all', () => {
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, query: '   ' }).tool).toBe('search_graph');
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, query: '   ', entityType: 'class' }).tool).toBe(
      'list_entities_by_type',
    );
  });

  it('lists by type when only a type is chosen', () => {
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, entityType: 'class' })).toEqual({
      tool: 'list_entities_by_type',
      params: { entity_type: 'class', limit: 200 },
    });
    expect(
      toolForFilter({ ...EMPTY_GRAPH_FILTER, entityType: 'class', sourceToolkit: 'code' }).params,
    ).toEqual({ entity_type: 'class', limit: 200, source_toolkit: 'code' });
  });

  it('lists by layer when a layer is chosen and no type is', () => {
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, layer: 'application' })).toEqual({
      tool: 'list_entities_by_layer',
      params: { layer: 'application', limit: 200 },
    });
    expect(
      toolForFilter({ ...EMPTY_GRAPH_FILTER, layer: 'application', sourceToolkit: 'code' }).params,
    ).toEqual({ layer: 'application', limit: 200, source_toolkit: 'code' });
  });

  it('lists by source when a source is the only filter', () => {
    expect(toolForFilter({ ...EMPTY_GRAPH_FILTER, sourceToolkit: 'code' })).toEqual({
      tool: 'list_entities_by_source',
      params: { source_toolkit: 'code', limit: 200 },
    });
  });

  it('prefers the type over the layer, so ONE read runs', () => {
    // Calling all four and merging would quadruple the load for one list, and
    // the four disagree about ordering, so the merge would have no order.
    expect(
      toolForFilter({ ...EMPTY_GRAPH_FILTER, entityType: 'class', layer: 'application' }).tool,
    ).toBe('list_entities_by_type');
  });
});

describe('useGraphBrowser', () => {
  it('reads the rows a search answered and reports which tool answered', async () => {
    const invokes = serveGraph({ query: 'checkout', results: [HIT], total: 1 });
    const { result } = renderHookWithProviders(() =>
      useGraphBrowser(TARGET, { ...EMPTY_GRAPH_FILTER, query: 'checkout' }),
    );

    await waitFor(() => {
      expect(result.current.entities).toHaveLength(1);
    });
    expect(result.current.tool).toBe('search_graph');
    expect(result.current.entities[0]).toMatchObject({
      id: 'code:checkout-service',
      name: 'CheckoutService',
      filePath: 'src/checkout/service.py',
    });
    expect(invokes[0]?.tool).toBe('search_graph');
    expect(invokes[0]?.parameters).toMatchObject({ query: 'checkout', output_format: 'json' });
  });

  it('reads a listing document, whose rows sit under a different key', async () => {
    serveGraph({ entities: [HIT], total: 1 });
    const { result } = renderHookWithProviders(() =>
      useGraphBrowser(TARGET, { ...EMPTY_GRAPH_FILTER, entityType: 'class' }),
    );
    await waitFor(() => {
      expect(result.current.entities).toHaveLength(1);
    });
    expect(result.current.tool).toBe('list_entities_by_type');
  });

  it('reports the provider own sentence for a refusal', async () => {
    serveGraph(undefined, 'No graph has been built for bucket graphs.');
    const { result } = renderHookWithProviders(() => useGraphBrowser(TARGET, EMPTY_GRAPH_FILTER));
    await waitFor(() => {
      expect(result.current.error).toBe('No graph has been built for bucket graphs.');
    });
    expect(result.current.entities).toEqual([]);
  });

  it('is NOT pending while the tab is off screen', async () => {
    // A disabled query stays `isPending` for ever in react-query; reporting it
    // as pending would leave a permanent "Reading the graph…" on a hidden tab.
    const invokes = serveGraph({ results: [] });
    const { result } = renderHookWithProviders(() =>
      useGraphBrowser(TARGET, EMPTY_GRAPH_FILTER, { enabled: false }),
    );
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(invokes).toEqual([]);
    expect(result.current.entities).toEqual([]);
  });
});

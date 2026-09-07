/**
 * The values the filter controls may offer.
 *
 * They come from `get_stats` — the same read, with the same arguments, that the
 * statistics panel makes — so the two features share ONE cache entry and ONE
 * invocation. Add an argument here, even a harmless one, and the key splits:
 * the load on the engine doubles for no visible difference on screen. That is
 * what the invoke count below pins.
 *
 * A filter must also never offer a value the graph does not hold. Hard-coding
 * the legacy type list would give a user fourteen choices of which two return
 * rows, and the twelve empty ones read as a broken graph rather than as an
 * inapplicable filter.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import { INVENTORY_FAMILY, useInventoryTool, type InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createTestQueryClient, renderHookWithProviders } from '../__tests__/testUtils';
import { useGraphFacets } from './useGraphFacets';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = { projectId: '7', toolkitId: '42', settings: { bucket: 'graphs' } };

const STATS = {
  node_count: 6,
  edge_count: 5,
  entities_by_type: { class: 3, function: 1, document: 2 },
  entities_by_layer: { application: 4, documentation: 2 },
  source_toolkits: ['code', 'docs'],
};

function serveStats(document: unknown): { readonly count: () => number } {
  let count = 0;
  server.use(
    http.post(INVOKE_ROUTE, () => {
      count += 1;
      return HttpResponse.json({ invocation_id: 'inv-1', status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, () =>
      HttpResponse.json({
        status: 'Completed',
        result: JSON.stringify([
          {
            object_type: 'message',
            result_target: 'response',
            data: JSON.stringify(document),
          },
        ]),
      }),
    ),
  );
  return { count: () => count };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useGraphFacets', () => {
  it('offers only the types, layers and sources this graph holds', async () => {
    serveStats(STATS);
    const { result } = renderHookWithProviders(() => useGraphFacets(TARGET));

    await waitFor(() => {
      expect(result.current.types).toHaveLength(3);
    });
    // Ordered by count, which is the order `readBreakdown` imposes so the list
    // does not change between two identical graphs.
    expect(result.current.types).toEqual(['class', 'document', 'function']);
    expect(result.current.layers).toEqual(['application', 'documentation']);
    expect(result.current.sources).toEqual(['code', 'docs']);
  });

  it('shares ONE invocation with any other reader of get_stats', async () => {
    // Two features asking the same question of the same toolkit are one unit of
    // work on the engine. A different argument here would split the key.
    const stats = serveStats(STATS);
    const client = createTestQueryClient();
    const { result } = renderHookWithProviders(
      () => ({
        facets: useGraphFacets(TARGET),
        direct: useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}),
      }),
      client,
    );

    await waitFor(() => {
      expect(result.current.facets.types).toHaveLength(3);
    });
    expect(result.current.direct.data?.document).toEqual(STATS);
    expect(stats.count()).toBe(1);
  });

  it('offers nothing rather than throwing for a graph with no statistics', async () => {
    serveStats({});
    const { result } = renderHookWithProviders(() => useGraphFacets(TARGET));
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(result.current.types).toEqual([]);
    expect(result.current.layers).toEqual([]);
    expect(result.current.sources).toEqual([]);
  });

  it('is not pending while the tab is off screen', async () => {
    const stats = serveStats(STATS);
    const { result } = renderHookWithProviders(() => useGraphFacets(TARGET, { enabled: false }));
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(stats.count()).toBe(0);
  });
});

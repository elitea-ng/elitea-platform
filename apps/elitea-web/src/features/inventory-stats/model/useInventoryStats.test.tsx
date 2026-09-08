/**
 * The graph's numbers, its cache, and the two tools that repair it.
 *
 * `get_stats` is read with the SAME arguments the browser reads it with, on
 * purpose: `useInventoryTool` keys a read by project, toolkit, family, tool and
 * arguments, so both features are one invocation. Adding an argument here —
 * even a harmless one — splits the key and doubles the load on the engine for
 * no visible difference, which is why the invoke count is asserted.
 *
 * Maintenance is a WRITE. `normalize_types` rewrites the stored graph, so every
 * read of the toolkit is stale afterwards; invalidating only this panel's two
 * queries leaves the browser listing types that no longer exist. The two tools
 * also deliberately do NOT ask for JSON — they answer a sentence, and that
 * sentence is what the panel shows.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, waitFor } from '@testing-library/react';

import { INVENTORY_FAMILY, useInventoryTool, type InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createTestQueryClient, renderHookWithProviders } from '../__tests__/testUtils';
import { useInventoryStats, useMaintenance } from './useInventoryStats';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = { projectId: '7', toolkitId: '42', settings: { bucket: 'graphs' } };

const STATS = {
  node_count: 6,
  edge_count: 5,
  entities_by_type: { class: 3, function: 1, document: 2 },
  entities_by_layer: { application: 4 },
  source_toolkits: ['code'],
};

const CACHE = { cached_graphs: 2, cache_size_bytes: 4096, hits: 9, misses: 1 };

interface Invoke {
  readonly tool: string;
  readonly parameters: Record<string, unknown>;
}

/** Answer each tool with its own text; `failing` refuses instead. */
function serveTools(texts: Record<string, string>, failing: readonly string[] = []): Invoke[] {
  const invokes: Invoke[] = [];
  server.use(
    http.post(INVOKE_ROUTE, async ({ params, request }) => {
      const body = (await request.json()) as { parameters: Record<string, unknown> };
      invokes.push({ tool: String(params['tool']), parameters: body.parameters });
      return HttpResponse.json({ invocation_id: String(params['tool']), status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, ({ params }) => {
      const tool = String(params['tool']);
      const refused = failing.includes(tool);
      const result = JSON.stringify([
        {
          object_type: 'message',
          result_target: 'response',
          data: refused ? `Inventory refused '${tool}'.` : (texts[tool] ?? ''),
        },
      ]);
      return HttpResponse.json(refused ? { status: 'Error', result } : { status: 'Completed', result });
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

describe('useInventoryStats', () => {
  it('reads the counts, the breakdowns and the cache', async () => {
    serveTools({
      get_stats: JSON.stringify(STATS),
      get_cache_stats: JSON.stringify(CACHE),
    });

    const { result } = renderHookWithProviders(() => useInventoryStats(TARGET));
    await waitFor(() => {
      expect(result.current.stats.nodeCount).toBe(6);
    });
    await waitFor(() => {
      expect(result.current.cachedGraphs).toBe(2);
    });
    expect(result.current.stats.byType).toEqual([
      { name: 'class', count: 3 },
      { name: 'document', count: 2 },
      { name: 'function', count: 1 },
    ]);
    expect(result.current.cacheSizeBytes).toBe(4096);
  });

  it('shares ONE get_stats invocation with the browser facets', async () => {
    const invokes = serveTools({ get_stats: JSON.stringify(STATS), get_cache_stats: '{}' });
    const client = createTestQueryClient();
    renderHookWithProviders(
      () => ({
        panel: useInventoryStats(TARGET),
        browser: useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}),
      }),
      client,
    );

    await waitFor(() => {
      expect(invokes.filter((invoke) => invoke.tool === 'get_cache_stats')).toHaveLength(1);
    });
    expect(invokes.filter((invoke) => invoke.tool === 'get_stats')).toHaveLength(1);
  });

  it('still shows the counts when the CACHE read fails', async () => {
    // The two are separate facts, and a graph's size is worth showing without
    // its cache usage.
    serveTools({ get_stats: JSON.stringify(STATS) }, ['get_cache_stats']);
    const { result } = renderHookWithProviders(() => useInventoryStats(TARGET));
    await waitFor(() => {
      expect(result.current.stats.nodeCount).toBe(6);
    });
    expect(result.current.error).toBeNull();
    expect(result.current.cachedGraphs).toBeNull();
  });

  it('reports the provider sentence when the STATISTICS read fails', async () => {
    serveTools({ get_cache_stats: JSON.stringify(CACHE) }, ['get_stats']);
    const { result } = renderHookWithProviders(() => useInventoryStats(TARGET));
    await waitFor(() => {
      expect(result.current.error).toBe("Inventory refused 'get_stats'.");
    });
    expect(result.current.stats.nodeCount).toBeNull();
  });

  it('does not read while the tab is off screen', async () => {
    const invokes = serveTools({});
    const { result } = renderHookWithProviders(() => useInventoryStats(TARGET, { enabled: false }));
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(invokes).toEqual([]);
  });
});

describe('useMaintenance', () => {
  it('runs the tool WITHOUT asking for JSON, and shows the sentence it answered', async () => {
    // These two answer prose. Asking for JSON gives a document whose fields
    // differ between the fixture and the engine, and the screen would guess.
    const invokes = serveTools({
      normalize_types: 'Types are already normalised: 3 distinct types.',
    });

    const { result } = renderHookWithProviders(() => useMaintenance(TARGET));
    act(() => {
      result.current.run('normalize_types');
    });
    expect(result.current.running).toBe('normalize_types');

    await waitFor(() => {
      expect(result.current.message).toBe('Types are already normalised: 3 distinct types.');
    });
    expect(result.current.running).toBeNull();
    expect(invokes[0]).toEqual({ tool: 'normalize_types', parameters: {} });
  });

  it('invalidates EVERY read of the toolkit, not just this panel two', async () => {
    // Normalising rewrites the graph, so the entity lists and the facets are
    // stale as well.
    const invokes = serveTools({
      get_stats: JSON.stringify(STATS),
      rebuild_indices: 'Rebuilt 4 indices.',
    });
    const client = createTestQueryClient();
    const { result } = renderHookWithProviders(
      () => ({
        browser: useInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}),
        maintenance: useMaintenance(TARGET),
      }),
      client,
    );

    await waitFor(() => {
      expect(result.current.browser.data?.document).toEqual(STATS);
    });
    expect(invokes.filter((invoke) => invoke.tool === 'get_stats')).toHaveLength(1);

    act(() => {
      result.current.maintenance.run('rebuild_indices');
    });
    await waitFor(() => {
      expect(result.current.maintenance.message).toBe('Rebuilt 4 indices.');
    });
    await waitFor(() => {
      expect(invokes.filter((invoke) => invoke.tool === 'get_stats')).toHaveLength(2);
    });
  });

  it('reports a refused maintenance run and stops reporting it as running', async () => {
    serveTools({}, ['normalize_types']);
    const { result } = renderHookWithProviders(() => useMaintenance(TARGET));
    act(() => {
      result.current.run('normalize_types');
    });
    await waitFor(() => {
      expect(result.current.error).toBe("Inventory refused 'normalize_types'.");
    });
    expect(result.current.running).toBeNull();
    expect(result.current.message).toBeNull();
  });

  it('clears the previous answer when a second run starts', async () => {
    serveTools({ normalize_types: 'Normalised.', rebuild_indices: 'Rebuilt.' });
    const { result } = renderHookWithProviders(() => useMaintenance(TARGET));

    act(() => {
      result.current.run('normalize_types');
    });
    await waitFor(() => {
      expect(result.current.message).toBe('Normalised.');
    });

    act(() => {
      result.current.run('rebuild_indices');
    });
    expect(result.current.message).toBeNull();
    await waitFor(() => {
      expect(result.current.message).toBe('Rebuilt.');
    });
  });
});

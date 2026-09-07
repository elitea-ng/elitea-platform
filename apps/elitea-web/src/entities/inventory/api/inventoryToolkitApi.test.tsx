/**
 * Reading the Inventory toolkits a project holds, and one toolkit's settings.
 *
 * The toolkit IS the graph's address, so three failures here are total. Listing
 * `inventory_search` alongside `inventory` offers a workspace that can neither
 * read nor ingest, because the search family REFERENCES a graph it does not
 * own. Taking the row off the transport envelope rather than the body gives a
 * toolkit whose every field is undefined on a 200 (issue #132), which renders
 * as a toolkit with no sources and the default bucket. And a query left enabled
 * with an empty project or toolkit id fires at `/elitea_core/tool/prompt_lib//`
 * — a 404 that reads as a broken toolkit rather than as one not chosen yet.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  fetchInventoryToolkit,
  fetchSourceToolkitNames,
  listInventoryToolkits,
  useInventoryToolkit,
  useInventoryToolkits,
  useSourceToolkitNames,
} from './inventoryToolkitApi';

const BASE = 'http://elitea.test/api/v2';
const TOOLKIT_ROUTE = `${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`;
const LIST_ROUTE = `${BASE}/elitea_core/tools/prompt_lib/:projectId`;

/** Serve one toolkit row per id, and report which ids were asked for. */
function serveToolkits(rows: Record<string, unknown>): string[] {
  const seen: string[] = [];
  server.use(
    http.get(TOOLKIT_ROUTE, ({ params }) => {
      const id = String(params['toolkitId']);
      seen.push(id);
      const row = rows[id];
      return row === undefined
        ? HttpResponse.json({ error: 'toolkit not found' }, { status: 404 })
        : HttpResponse.json(row);
    }),
  );
  return seen;
}

/** Serve the project's toolkit listing, and report how often it was read. */
function serveList(rows: readonly Record<string, unknown>[]): { readonly count: () => number } {
  let count = 0;
  server.use(
    http.get(LIST_ROUTE, () => {
      count += 1;
      return HttpResponse.json({ rows, total: rows.length });
    }),
  );
  return { count: () => count };
}

function wrapper(): (props: { children: ReactNode }) => React.JSX.Element {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('fetchInventoryToolkit', () => {
  it('reads the settings the invocations are configured from', async () => {
    serveToolkits({
      '42': {
        id: 42,
        name: 'Service inventory',
        type: 'inventory',
        settings: { bucket: 'graphs', llm_model: 'gpt-5', sources: [9010] },
      },
    });

    const context = await fetchInventoryToolkit('7', '42');
    expect(context.name).toBe('Service inventory');
    expect(context.settings).toMatchObject({ bucket: 'graphs', llm_model: 'gpt-5' });
    expect(context.toolkit['id']).toBe(42);
  });

  it('reads toolkit_config when there is no settings object', async () => {
    // The settings are what drive every invocation: an empty object here sends
    // an invoke with no bucket and no model, which reads the default bucket and
    // refuses to ingest.
    serveToolkits({ '42': { id: 42, toolkit_config: { bucket: 'cfg-graphs' } } });
    const context = await fetchInventoryToolkit('7', '42');
    expect(context.settings).toEqual({ bucket: 'cfg-graphs' });
  });

  it('answers empty settings rather than an array or a scalar', async () => {
    serveToolkits({ '42': { id: 42, settings: [], toolkit_config: 'graphs' } });
    await expect(fetchInventoryToolkit('7', '42')).resolves.toMatchObject({ settings: {} });
  });

  it('names an unnamed toolkit by its id, which is its address', async () => {
    // Hiding a row behind a missing label would make a graph unreachable.
    serveToolkits({ '42': { id: 42, settings: {} } });
    await expect(fetchInventoryToolkit('7', '42')).resolves.toMatchObject({ name: '42' });
  });

  it('survives a body that is not a row at all', async () => {
    server.use(http.get(TOOLKIT_ROUTE, () => HttpResponse.json(null)));
    await expect(fetchInventoryToolkit('7', '42')).resolves.toMatchObject({ name: '42', settings: {} });
  });
});

describe('listInventoryToolkits', () => {
  it('keeps the OWNING type only, and reads it off the envelope', async () => {
    // `inventory_search` references a graph another toolkit built; offering it
    // as a workspace gives the user a screen that can read nothing.
    serveList([
      { id: 1, type: 'github', name: 'Code' },
      { id: 42, type: 'inventory', name: 'Service inventory' },
      { id: 43, type: 'Inventory', name: 'Docs inventory' },
      { id: 44, type: 'inventory_search', name: 'Read-only' },
    ]);

    await expect(listInventoryToolkits('7')).resolves.toEqual([
      { id: '42', name: 'Service inventory' },
      { id: '43', name: 'Docs inventory' },
    ]);
  });

  it('falls back to the id when a row has no name', async () => {
    serveList([{ id: 42, type: 'inventory' }]);
    await expect(listInventoryToolkits('7')).resolves.toEqual([{ id: '42', name: '42' }]);
  });

  it('drops a row with no id, which has no address to open', async () => {
    serveList([{ type: 'inventory', name: 'nowhere' }]);
    await expect(listInventoryToolkits('7')).resolves.toEqual([]);
  });

  it('reads an empty listing as no inventories rather than throwing', async () => {
    serveList([]);
    await expect(listInventoryToolkits('7')).resolves.toEqual([]);
  });

  it('does not match a row whose type is an object', async () => {
    // `String(someObject)` is "[object Object]"; a row named after the failure
    // would be offered as a toolkit.
    serveList([{ id: 42, type: { name: 'inventory' } }]);
    await expect(listInventoryToolkits('7')).resolves.toEqual([]);
  });
});

describe('fetchSourceToolkitNames', () => {
  it('resolves every source label in ONE request', async () => {
    // A bare id is not something a user recognises, and a project with eight
    // sources would otherwise make eight requests to render eight labels.
    const list = serveList([
      { id: 9010, name: 'Checkout repo', type: 'github' },
      { id: 9011, type: 'confluence' },
      { name: 'no id here', type: 'github' },
    ]);

    const names = await fetchSourceToolkitNames('7');
    expect(list.count()).toBe(1);
    expect(names.get('9010')).toEqual({ name: 'Checkout repo', type: 'github' });
    expect(names.get('9011')).toEqual({ name: '9011', type: 'confluence' });
    expect(names.size).toBe(2);
  });
});

describe('the toolkit hooks', () => {
  it('reads one toolkit through useInventoryToolkit', async () => {
    serveToolkits({ '42': { id: 42, name: 'Service inventory', settings: { bucket: 'graphs' } } });
    const { result } = renderHook(() => useInventoryToolkit('7', '42'), { wrapper: wrapper() });
    await waitFor(() => {
      expect(result.current.data?.name).toBe('Service inventory');
    });
  });

  it('stays IDLE while either half of the address is missing', async () => {
    // Enabled with an empty id, the read fires at `/tool/prompt_lib//` and
    // 404s, which the screen shows as a broken toolkit.
    const seen = serveToolkits({});
    const { result } = renderHook(() => useInventoryToolkit('7', ''), { wrapper: wrapper() });
    await waitFor(() => {
      expect(result.current.fetchStatus).toBe('idle');
    });
    expect(seen).toEqual([]);
  });

  it('lists the project inventories through useInventoryToolkits', async () => {
    serveList([{ id: 42, type: 'inventory', name: 'Service inventory' }]);
    const { result } = renderHook(() => useInventoryToolkits('7'), { wrapper: wrapper() });
    await waitFor(() => {
      expect(result.current.data).toEqual([{ id: '42', name: 'Service inventory' }]);
    });
  });

  it('keeps the listing hooks idle for an unresolved project', async () => {
    const list = serveList([]);
    const { result } = renderHook(
      () => ({ toolkits: useInventoryToolkits(''), names: useSourceToolkitNames('') }),
      { wrapper: wrapper() },
    );
    await waitFor(() => {
      expect(result.current.toolkits.fetchStatus).toBe('idle');
    });
    expect(result.current.names.fetchStatus).toBe('idle');
    expect(list.count()).toBe(0);
  });

  it('resolves the source names through useSourceToolkitNames', async () => {
    serveList([{ id: 9010, name: 'Checkout repo', type: 'github' }]);
    const { result } = renderHook(() => useSourceToolkitNames('7'), { wrapper: wrapper() });
    await waitFor(() => {
      expect(result.current.data?.get('9010')?.name).toBe('Checkout repo');
    });
  });
});

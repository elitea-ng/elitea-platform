import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { getGetAgentCategoriesMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../test/setup';

import { ALL_AGENTS_LIMIT } from './constants';
import type { AgentHubSortBy } from './types';
import { useAgentHubData } from './useAgentHubData';

const globals = globalThis as unknown as Record<string, unknown>;

const BASE = '/api/v2';

function setConfig(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

/** `useGetAgentCategories` (the generated `useQuery` hook) needs a `QueryClientProvider` ancestor. */
function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

/**
 * The path is pinned to the FULL `/api/v2` base, and the body is the real
 * top-level `{rows, total}` the handler sends
 * (`internal/api/v2/eliteacore/handler.go`).
 *
 * DEFECT this guards: `fetchAllApplications` used a raw `fetch` with a bare
 * `/elitea_core/...` path (404 — no `/api/v2` base). It then read
 * `json.data.rows` from a body that carries `rows` at the top level. The old
 * handler here hid both bugs. A `*` wildcard path matches the un-prefixed
 * URL. The mock body wrapped the rows in the `data` key the server never
 * sends.
 */
function mockBulkList(rows: unknown[]): void {
  server.use(
    http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, () =>
      HttpResponse.json({ rows, total: rows.length }, { status: 200 }),
    ),
  );
}

describe('useAgentHubData', () => {
  afterEach(() => {
    delete globals['elitea_ui_config'];
    resetConfigForTests();
    resetGeneratedClient();
  });

  it('fetches agent_categories for the configured VITE_PUBLIC_PROJECT_ID, not a hardcoded "1" (adversarial-review fix, cluster A13-agents-hub, finding 7)', async () => {
    setConfig('77');
    configureGeneratedClient({ baseUrl: BASE });
    let requestedProjectId: string | undefined;
    server.use(
      http.get('*/elitea_core/agent_categories/prompt_lib/:projectId', ({ params }) => {
        requestedProjectId = params['projectId'] as string;
        return HttpResponse.json({ categories: [{ name: 'Productivity', is_default: true }], total: 1 });
      }),
    );
    mockBulkList([]);

    renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(requestedProjectId).toBe('77'));
  });

  it('buckets fetched agents by meta.category (adversarial-review fix, cluster A13-agents-hub, finding 4, exercised end-to-end through this hook)', async () => {
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));
    mockBulkList([
      {
        project_id: '1',
        id: 'app-1',
        name: 'Research Agent',
        description: '',
        version_id: 'v-1',
        version_name: 'v1',
        agent_type: 'agent',
        meta: { category: 'Productivity' },
      },
    ]);

    const { result } = renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(result.current.applicationsByTag['Productivity']).toHaveLength(1));
    expect(result.current.applicationsByTag['Productivity']?.[0]?.id).toBe('app-1');
  });

  it('requests the /api/v2-based public_applications path and reads rows from the top level of the body', async () => {
    // DEFECT: `fetchAllApplications` used a raw `fetch`, so the request lost
    // the `/api/v2` base and 404'd. It then read `json.data.rows` from a body
    // whose real shape is `{"rows":[],"total":0}`. Either bug alone leaves
    // every Agent Hub category empty.
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));

    let requestedUrl = '';
    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, ({ request }) => {
        requestedUrl = request.url;
        return HttpResponse.json(
          {
            rows: [
              {
                project_id: '1',
                id: 'app-1',
                name: 'Research Agent',
                description: '',
                version_id: 'v-1',
                version_name: 'v1',
                agent_type: 'agent',
                meta: { category: 'Productivity' },
              },
            ],
            total: 1,
          },
          { status: 200 },
        );
      }),
    );

    const { result } = renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(result.current.applicationsByTag['Productivity']).toHaveLength(1));
    expect(new URL(requestedUrl).pathname).toBe('/api/v2/elitea_core/public_applications/prompt_lib');
  });

  it('exposes the list failure as an error instead of an empty hub and an unhandled rejection', async () => {
    // DEFECT: the three fetches used `try { … } finally { … }` with no
    // `catch`, and the effect discarded each promise with `void`. A 403 on
    // `models.applications.application.list` therefore cleared the loading
    // flag, left `applicationsByTag` empty, and surfaced only as an unhandled
    // promise rejection. The page showed the ordinary "No agents found" empty
    // state. A refusal was therefore indistinguishable from an empty catalog.
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));
    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, () =>
        HttpResponse.json({ error: 'forbidden' }, { status: 403 }),
      ),
    );

    const rejections: unknown[] = [];
    const onRejection = (event: PromiseRejectionEvent) => {
      rejections.push(event.reason);
      event.preventDefault();
    };
    window.addEventListener('unhandledrejection', onRejection);

    try {
      const { result } = renderHook(() => useAgentHubData([]), { wrapper });

      await waitFor(() => expect(result.current.error).toBeInstanceOf(Error));
      expect(result.current.isFetching).toBe(false);
      expect(result.current.error?.message).toContain('403');
      // A rejection that escapes the effect never reaches an error boundary.
      await act(async () => { await Promise.resolve(); });
      expect(rejections).toHaveLength(0);
    } finally {
      window.removeEventListener('unhandledrejection', onRejection);
    }
  });

  it('surfaces a categories failure too, which the fetch effect skips over', async () => {
    // DEFECT: when `agent_categories` fails, `categoryNames` is empty and the
    // fetch effect returns early, so no list request ever runs. Before the
    // fix nothing carried that refusal out of the hook and the hub rendered
    // empty with no reason given.
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/elitea_core/agent_categories/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'forbidden' }, { status: 403 }),
      ),
    );
    mockBulkList([]);

    const { result } = renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(result.current.error).toBeInstanceOf(Error));
    expect(result.current.categoryNames).toHaveLength(0);
  });

  it('still sends sort_by=likes/sort_order=desc for the Trending bucket and my_liked=true for My Liked (forward-compat with the disclosed backend gap — findings 5 & 6)', async () => {
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));

    const seenQueries: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, ({ request }) => {
        seenQueries.push(new URL(request.url).search);
        return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
      }),
    );

    renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(seenQueries.length).toBeGreaterThanOrEqual(3));
    expect(seenQueries.some(q => q.includes('sort_by=likes') && q.includes('sort_order=desc'))).toBe(true);
    expect(seenQueries.some(q => q.includes('my_liked=true'))).toBe(true);
  });

  it('filters applicationsByTag down to only the selected tag names when any are selected', async () => {
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      getGetAgentCategoriesMockHandler({
        categories: [
          { name: 'Productivity', is_default: true },
          { name: 'Support', is_default: true },
        ],
        total: 2,
      }),
    );
    mockBulkList([
      {
        project_id: '1',
        id: 'app-1',
        name: 'Research Agent',
        description: '',
        version_id: 'v-1',
        version_name: 'v1',
        agent_type: 'agent',
        meta: { category: 'Productivity' },
      },
      {
        project_id: '1',
        id: 'app-2',
        name: 'Support Bot',
        description: '',
        version_id: 'v-2',
        version_name: 'v1',
        agent_type: 'agent',
        meta: { category: 'Support' },
      },
    ]);

    const { result, rerender } = renderHook(({ tags }: { tags: string[] }) => useAgentHubData(tags), {
      initialProps: { tags: [] as string[] },
      wrapper,
    });

    await waitFor(() => expect(Object.keys(result.current.applicationsByTag).length).toBeGreaterThan(0));

    rerender({ tags: ['Support'] });

    await act(async () => {});
    expect(result.current.applicationsByTag['Support']).toBeDefined();
    expect(result.current.applicationsByTag['Productivity']).toBeUndefined();
  });
  /* ── Issue #36 item 8: the parameters the catalogue handler now reads ── */

  it('sends the search text as ?query= and the sort as ?sort_by=/?sort_order=, and re-asks when they change', async () => {
    // The old hook filtered by name in the browser over whatever page was in
    // memory, so a match past the row cap read as "No agents found". The
    // handler filters and sorts now, so the request must carry both.
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));

    const bulkQueries: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, ({ request }) => {
        const params = new URL(request.url).searchParams;
        // The two special buckets have their own markers; only the bulk page
        // carries the caller's sort.
        if (params.get('my_liked') === null && params.get('sort_by') !== 'likes') {
          bulkQueries.push(request.url);
        }
        return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
      }),
    );

    const { rerender } = renderHook(
      ({ options }: { options: { query: string; sortBy: AgentHubSortBy } }) =>
        useAgentHubData([], { query: options.query, sortBy: options.sortBy, sortOrder: 'asc' }),
      { initialProps: { options: { query: 'release', sortBy: 'name' } }, wrapper },
    );

    await waitFor(() => expect(bulkQueries.length).toBeGreaterThanOrEqual(1));
    const first = new URL(bulkQueries[0] as string).searchParams;
    expect(first.get('query')).toBe('release');
    expect(first.get('sort_by')).toBe('name');
    expect(first.get('sort_order')).toBe('asc');
    expect(first.get('limit')).toBe(String(ALL_AGENTS_LIMIT));
    expect(first.get('offset')).toBe('0');

    rerender({ options: { query: 'triage', sortBy: 'created_at' } });

    await waitFor(() => expect(bulkQueries.length).toBeGreaterThanOrEqual(2));
    const second = new URL(bulkQueries[bulkQueries.length - 1] as string).searchParams;
    expect(second.get('query')).toBe('triage');
    expect(second.get('sort_by')).toBe('created_at');
  });

  it('omits ?query= entirely when the search box is empty', async () => {
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));
    const seen: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, ({ request }) => {
        seen.push(request.url);
        return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
      }),
    );

    renderHook(() => useAgentHubData([], { query: '   ' }), { wrapper });

    await waitFor(() => expect(seen.length).toBeGreaterThanOrEqual(1));
    // An empty `query=` would be a search for the empty string, which the
    // handler reads as "no filter" only because it trims — do not rely on it.
    expect(seen.every(url => new URL(url).searchParams.get('query') === null)).toBe(true);
  });

  it('pages with limit/offset and stops when the rows in hand reach the server total', async () => {
    setConfig('1');
    configureGeneratedClient({ baseUrl: BASE });
    server.use(getGetAgentCategoriesMockHandler({ categories: [{ name: 'Productivity', is_default: true }], total: 1 }));

    const row = (id: string) => ({
      project_id: '1',
      id,
      name: `Agent ${id}`,
      description: '',
      version_id: `v-${id}`,
      version_name: 'v1',
      agent_type: 'agent',
      meta: { category: 'Productivity' },
      tags: [],
      likes: 0,
      is_liked: false,
    });

    server.use(
      http.get(`${BASE}/elitea_core/public_applications/prompt_lib`, ({ request }) => {
        const params = new URL(request.url).searchParams;
        if (params.get('my_liked') !== null || params.get('sort_by') === 'likes') {
          return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
        }
        // Two rows in the set, one per page, so "load more" has exactly one
        // page left to ask for and then must stop.
        const offset = Number(params.get('offset') ?? '0');
        return HttpResponse.json(
          { rows: offset === 0 ? [row('a')] : [row('b')], total: 2 },
          { status: 200 },
        );
      }),
    );

    const { result } = renderHook(() => useAgentHubData([]), { wrapper });

    await waitFor(() => expect(result.current.applicationsByTag['Productivity']).toHaveLength(1));
    expect(result.current.hasMore).toBe(true);
    expect(result.current.totalCount).toBe(2);

    await act(async () => { await result.current.loadMore(); });

    await waitFor(() => expect(result.current.applicationsByTag['Productivity']).toHaveLength(2));
    expect(result.current.hasMore).toBe(false);

    // A second call has nothing left to ask for and must not append a
    // duplicate page.
    await act(async () => { await result.current.loadMore(); });
    expect(result.current.applicationsByTag['Productivity']).toHaveLength(2);
  });
});

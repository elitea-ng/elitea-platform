/**
 * indexesApi.test.ts — contract + hook coverage for `./indexesApi.ts` (unit
 * A4a). Every test asserts against a real `eliteaFetch` call (MSW-mocked,
 * no `vi.mock()` of application code, matching R-M1 / `configurations.test.ts`'s
 * own convention), and hook tests use a real `QueryClient` +
 * `useIndexesStore` singleton (reset per test).
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { server } from '../../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { useIndexesStore } from '../model/indexesStore';

import {
  deleteIndexItem,
  getIndexHistoryConversationDetails,
  getIndexSchedule,
  getIndexesList,
  startIndexExecution,
  stopIndexingItem,
  saveIndexConfiguration,
  updateIndexSchedule,
  useDeleteIndexItemMutation,
  useIndexHistoryConversationDetailsQuery,
  useIndexScheduleQuery,
  useIndexesListQuery,
  useStopIndexingItemMutation,
  useUpdateIndexScheduleMutation,
} from './indexesApi';

const BASE = '/api/v2';

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('getIndexesList', () => {
  it('GETs elitea_core/index_meta/prompt_lib/{projectId}/{toolkitId}', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1`, () =>
        HttpResponse.json([{ id: 'i1', metadata: { collection: 'idx' } }]),
      ),
    );
    const result = await getIndexesList({ toolkitId: 'tk-1', projectId: 7 });
    expect(result).toEqual([{ id: 'i1', metadata: { collection: 'idx' } }]);
  });
});

describe('useIndexesListQuery', () => {
  it('stays disabled until toolkitId and projectId are both defined', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useIndexesListQuery({ toolkitId: undefined, projectId: 7 }), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });

  it('fetches once both ids are present', async () => {
    server.use(http.get(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1`, () => HttpResponse.json([])));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useIndexesListQuery({ toolkitId: 'tk-1', projectId: 7 }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toEqual([]);
  });
});

describe('deleteIndexItem', () => {
  it('DELETEs with is_hidden:true in the body', async () => {
    let capturedBody: unknown;
    server.use(
      http.delete(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/idx-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    await deleteIndexItem({ projectId: 7, toolkitId: 'tk-1', indexId: 'idx-1', indexName: 'my-index' });
    expect(capturedBody).toEqual({ is_hidden: true });
  });
});

describe('useDeleteIndexItemMutation', () => {
  it('on success, removes the deleted index name from the schedule store', async () => {
    useIndexesStore.getState().setToolkitScheduler({ 'my-index': { enabled: true }, other: { enabled: false } });
    server.use(http.delete(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/idx-1`, () => HttpResponse.json({})));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useDeleteIndexItemMutation(), { wrapper });
    result.current.mutate({ projectId: 7, toolkitId: 'tk-1', indexId: 'idx-1', indexName: 'my-index' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(useIndexesStore.getState().toolkitScheduler).toEqual({ other: { enabled: false } });
  });
});

describe('stopIndexingItem / useStopIndexingItemMutation', () => {
  it('DELETEs elitea_core/index_cancel/.../{indexName}/{taskId}', async () => {
    server.use(http.delete(`${BASE}/elitea_core/index_cancel/prompt_lib/7/tk-1/my-index/task-9`, () => HttpResponse.json({})));
    await expect(
      stopIndexingItem({ projectId: 7, toolkitId: 'tk-1', indexName: 'my-index', taskId: 'task-9' }),
    ).resolves.toEqual({});
  });

  it('mutation resolves via the hook', async () => {
    server.use(http.delete(`${BASE}/elitea_core/index_cancel/prompt_lib/7/tk-1/my-index/task-9`, () => HttpResponse.json({})));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useStopIndexingItemMutation(), { wrapper });
    result.current.mutate({ projectId: 7, toolkitId: 'tk-1', indexName: 'my-index', taskId: 'task-9' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
  });
});

describe('updateIndexSchedule', () => {
  it('PATCHes with cron/enabled/credentials + timezone in the body, indexName kept out of the body', async () => {
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/my-index`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    await updateIndexSchedule({
      projectId: 7,
      toolkitId: 'tk-1',
      indexName: 'my-index',
      timezone: 'UTC',
      cron: '0 0 * * 6',
      enabled: true,
      credentials: 'cred-1',
    });
    expect(capturedBody).toEqual({ timezone: 'UTC', cron: '0 0 * * 6', enabled: true, credentials: 'cred-1' });
  });
});

/**
 * ELITEA-2880 — the SAVE half of the Indexes tab's Save / Save & Reindex
 * split. The URL is asserted because the client builds it by interpolation
 * and the index segment carries a NAME, which may contain characters a path
 * segment cannot.
 */
describe('saveIndexConfiguration', () => {
  it('PUTs the configuration under an index_configuration key, at the per-index configuration path', async () => {
    let capturedBody: unknown;
    let capturedUrl = '';
    server.use(
      http.put(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/:indexName/configuration`, async ({ request }) => {
        capturedBody = await request.json();
        capturedUrl = new URL(request.url).pathname;
        return HttpResponse.json({ ok: true });
      }),
    );
    await saveIndexConfiguration({
      projectId: 7,
      toolkitId: 'tk-1',
      indexName: 'my index/name',
      configuration: { progress_step: 75, folder: undefined },
    });
    expect(capturedUrl).toBe(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/my%20index%2Fname/configuration`);
    // `folder: undefined` survives as an explicit `null` (#311): a cleared
    // field that vanished from the body would come back re-defaulted.
    expect(capturedBody).toEqual({ index_configuration: { progress_step: 75, folder: null } });
  });
});

describe('useUpdateIndexScheduleMutation', () => {
  it('invalidates the index-schedule query scope for the toolkit on success', async () => {
    server.use(http.patch(`${BASE}/elitea_core/index_meta/prompt_lib/7/tk-1/my-index`, () => HttpResponse.json({})));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    }
    const { result } = renderHook(() => useUpdateIndexScheduleMutation(), { wrapper: Wrapper });
    result.current.mutate({ projectId: 7, toolkitId: 'tk-1', indexName: 'my-index', timezone: 'UTC', enabled: true });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(client.getQueryState(['toolkits', 'indexSchedule', 'tk-1'])?.isInvalidated ?? true).toBeTruthy();
  });
});

describe('getIndexSchedule', () => {
  it('GETs elitea_core/tool/prompt_lib/{projectId}/{toolkitId}', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/tool/prompt_lib/7/tk-1`, () =>
        HttpResponse.json({ meta: { indexes_meta: { 'my-index': { enabled: true } } } }),
      ),
    );
    const result = await getIndexSchedule({ projectId: 7, toolkitId: 'tk-1' });
    expect(result.meta?.indexes_meta).toEqual({ 'my-index': { enabled: true } });
  });
});

describe('useIndexScheduleQuery', () => {
  it('on success, mirrors meta.indexes_meta into the store toolkitScheduler', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/tool/prompt_lib/7/tk-1`, () =>
        HttpResponse.json({ meta: { indexes_meta: { 'my-index': { enabled: true, cron: '0 0 * * 6' } } } }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useIndexScheduleQuery({ projectId: 7, toolkitId: 'tk-1' }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(useIndexesStore.getState().toolkitScheduler).toEqual({ 'my-index': { enabled: true, cron: '0 0 * * 6' } });
  });

  it('defaults to an empty scheduler map when meta.indexes_meta is absent', async () => {
    server.use(http.get(`${BASE}/elitea_core/tool/prompt_lib/7/tk-1`, () => HttpResponse.json({})));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useIndexScheduleQuery({ projectId: 7, toolkitId: 'tk-1' }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(useIndexesStore.getState().toolkitScheduler).toEqual({});
  });
});

describe('getIndexHistoryConversationDetails / useIndexHistoryConversationDetailsQuery', () => {
  it('GETs elitea_core/conversation/prompt_lib/{projectId}/{conversationId}', async () => {
    server.use(http.get(`${BASE}/elitea_core/conversation/prompt_lib/7/conv-1`, () => HttpResponse.json({ id: 'conv-1' })));
    const result = await getIndexHistoryConversationDetails({ projectId: 7, conversationId: 'conv-1' });
    expect(result).toEqual({ id: 'conv-1' });
  });

  it('hook stays disabled until conversationId is defined', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(
      () => useIndexHistoryConversationDetailsQuery({ projectId: 7, conversationId: undefined }),
      { wrapper },
    );
    expect(result.current.fetchStatus).toBe('idle');
  });

  it('hook can be explicitly disabled even with both ids present', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(
      () => useIndexHistoryConversationDetailsQuery({ projectId: 7, conversationId: 'conv-1' }, { enabled: false }),
      { wrapper },
    );
    expect(result.current.fetchStatus).toBe('idle');
  });
});

describe('startIndexExecution (issue #93)', () => {
  it('POSTs the GO contract body — toolkit_config + tool_name index_data — with await_response=false and the index.ingest.v1 contract', async () => {
    let seenUrl: string | undefined;
    let seenBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/test_toolkit_tool/prompt_lib/7`, async ({ request }) => {
        seenUrl = request.url;
        seenBody = await request.json();
        return HttpResponse.json({ task_id: 'exec-1' });
      }),
    );

    await expect(
      startIndexExecution({
        projectId: 7,
        toolkitId: 'tk-1',
        toolParams: { index_name: 'docs' },
        llmModel: 'gpt-4o-mini',
        llmSettings: { temperature: 0.1 },
      }),
    ).resolves.toEqual({ task_id: 'exec-1' });

    const query = new URL(String(seenUrl)).searchParams;
    expect(query.get('await_response')).toBe('false');
    expect(query.get('execution_contract')).toBe('index.ingest.v1');
    // Field-for-field against `currentStartBody` in
    // services/elitea-main/internal/api/v2/indexing/start_handler.go —
    // `toolkit_config` and a `tool_name` of exactly `index_data` are what
    // the handler validates before anything else.
    expect(seenBody).toEqual({
      toolkit_config: { toolkit_id: 'tk-1' },
      tool_name: 'index_data',
      tool_params: { index_name: 'docs' },
      llm_model: 'gpt-4o-mini',
      llm_settings: { temperature: 0.1 },
    });
  });

  it('omits llm_model / llm_settings when unset, letting the Go handler apply its own defaults', async () => {
    let seenBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/test_toolkit_tool/prompt_lib/7`, async ({ request }) => {
        seenBody = await request.json();
        return HttpResponse.json({ task_id: 'exec-1' });
      }),
    );
    await startIndexExecution({ projectId: 7, toolkitId: 'tk-1', toolParams: {} });
    expect(seenBody).toEqual({ toolkit_config: { toolkit_id: 'tk-1' }, tool_name: 'index_data', tool_params: {} });
  });

  it('rejects when the route is not mounted — the caller decides to fall back, not this function', async () => {
    server.use(http.post(`${BASE}/elitea_core/test_toolkit_tool/prompt_lib/7`, () => new HttpResponse(null, { status: 404 })));
    await expect(startIndexExecution({ projectId: 7, toolkitId: 'tk-1', toolParams: {} })).rejects.toThrow();
  });

  /**
   * #311, the save-path half. A field the user cleared arrives here as a
   * PRESENT `tool_params` key holding `undefined` (`ToolFormContainer`'s own
   * fix, `resolveFieldValue`). Plain `JSON.stringify` would drop that key —
   * the field would reach the Go handler indistinguishable from "never set",
   * and a later fetch of the saved index configuration would carry the same
   * gap, so `resolveFieldValue` reapplies the schema default on reload. This
   * test reads the ACTUAL body MSW received off the wire (not the JS object
   * passed in), which is the only way to catch a `JSON.stringify` key drop —
   * asserting against the pre-serialization object cannot reach this defect.
   */
  it('keeps a cleared tool_params field on the wire as an explicit null, not a dropped key', async () => {
    let seenBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/test_toolkit_tool/prompt_lib/7`, async ({ request }) => {
        seenBody = await request.json();
        return HttpResponse.json({ task_id: 'exec-1' });
      }),
    );

    await startIndexExecution({
      projectId: 7,
      toolkitId: 'tk-1',
      toolParams: { index_name: 'docs', output_format: undefined },
    });

    const toolParams = (seenBody as { tool_params: Record<string, unknown> }).tool_params;
    expect(Object.hasOwn(toolParams, 'output_format')).toBe(true);
    expect(toolParams['output_format']).toBeNull();
    expect(toolParams['output_format']).not.toBe('json');
  });
});

import { waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createTestQueryClient, renderHookWithProviders } from '../__tests__/testUtils';
import { useToolkitCreate, useToolkitEdit, useToolkitDelete, useToolkitDetail, useToolkitTypes, useToolkitsList } from './toolkits';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useToolkitTypes', () => {
  it('resolves the real toolkit-type schema catalogue', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));

    const { result } = renderHookWithProviders(() => useToolkitTypes({ projectId: 'proj-1' }));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.toolkitTypes).toEqual({ github: { metadata: { label: 'GitHub' } } });
    expect(result.current.isError).toBe(false);
  });

  it('does not fetch while no project is selected', async () => {
    const { result } = renderHookWithProviders(() => useToolkitTypes({ projectId: undefined }));
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.toolkitTypes).toBeUndefined();
  });
});

describe('useToolkitsList', () => {
  it('resolves a page of raw toolkit-instance rows and the real total', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [{ id: 'tk-1', type: 'github', name: 'my-github', description: '', settings: {}, meta: {}, created_at: '2026-01-01T00:00:00Z', author_id: 1 }], total: 1 }),
      ),
    );

    const { result } = renderHookWithProviders(() => useToolkitsList({ projectId: 'proj-1', page: 0, pageSize: 20 }));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.rows).toHaveLength(1);
    expect(result.current.rows[0]?.id).toBe('tk-1');
    expect(result.current.total).toBe(1);
  });

  it('refetch triggers a second network request', async () => {
    let requestCount = 0;
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => {
        requestCount += 1;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );

    const { result } = renderHookWithProviders(() => useToolkitsList({ projectId: 'proj-1', page: 0, pageSize: 20 }));
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(requestCount).toBe(1);

    result.current.refetch();

    await waitFor(() => expect(requestCount).toBe(2));
  });
});

describe('useToolkitDetail', () => {
  it('calls the real GET-single endpoint directly, not the paginated list', async () => {
    let listRequestCount = 0;
    let detailRequestedProjectId: string | undefined;
    let detailRequestedToolkitId: string | undefined;
    server.use(
      // A regression here (silently reverting to the list-based approximation)
      // would fail this test: the mock below only serves the GET-single path,
      // and separately counts hits against the list endpoint to prove it is
      // never called.
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => {
        listRequestCount += 1;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
      http.get('/api/v2/elitea_core/tool/prompt_lib/:projectId/:toolkitId', ({ params }) => {
        detailRequestedProjectId = params['projectId'] as string;
        detailRequestedToolkitId = params['toolkitId'] as string;
        return HttpResponse.json({
          id: 'tk-2',
          type: 'jira',
          name: 'b',
          description: '',
          settings: {},
          meta: {},
          created_at: '2026-01-01T00:00:00Z',
          author_id: 1,
        });
      }),
    );

    const { result } = renderHookWithProviders(() => useToolkitDetail({ projectId: 'proj-1', toolkitId: 'tk-2' }));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.detail?.id).toBe('tk-2');
    expect(result.current.detail?.type).toBe('jira');
    expect(detailRequestedProjectId).toBe('proj-1');
    expect(detailRequestedToolkitId).toBe('tk-2');
    expect(listRequestCount).toBe(0);
  });

  it('resolves a toolkit whose id would not appear on the first page of the instance list', async () => {
    // Regression guard for the "200-row page cap" bug: a toolkit past
    // MAX_DETAIL_LOOKUP_PAGE_SIZE (200) in the old list-scan approach would
    // never be found because the client-side .find() only searched page 0.
    // The GET-single endpoint has no such limit — it resolves by id directly.
    server.use(
      http.get('/api/v2/elitea_core/tool/prompt_lib/:projectId/:toolkitId', ({ params }) =>
        HttpResponse.json({
          id: params['toolkitId'] as string,
          type: 'confluence',
          name: 'row-201',
          description: '',
          settings: {},
          meta: {},
          created_at: '2026-01-01T00:00:00Z',
          author_id: 1,
        }),
      ),
    );

    const { result } = renderHookWithProviders(() => useToolkitDetail({ projectId: 'proj-1', toolkitId: 'tk-201' }));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.detail?.id).toBe('tk-201');
    expect(result.current.isError).toBe(false);
  });

  it('resolves undefined and does not fetch while projectId or toolkitId is missing', async () => {
    const { result } = renderHookWithProviders(() => useToolkitDetail({ projectId: 'proj-1', toolkitId: undefined }));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.detail).toBeUndefined();
    expect(result.current.isError).toBe(false);
  });
});

describe('useToolkitDelete', () => {
  it('issues a real DELETE against the tool endpoint and resolves', async () => {
    let deletedProjectId: string | undefined;
    let deletedToolId: string | undefined;
    server.use(
      http.delete('/api/v2/elitea_core/tool/prompt_lib/:projectId/:toolId', ({ params }) => {
        deletedProjectId = params['projectId'] as string;
        deletedToolId = params['toolId'] as string;
        return HttpResponse.json({});
      }),
    );

    const queryClient = createTestQueryClient();
    const { result } = renderHookWithProviders(() => useToolkitDelete(), queryClient);

    await result.current.deleteToolkit({ projectId: 'proj-1', toolkitId: '42' });

    expect(deletedProjectId).toBe('proj-1');
    expect(deletedToolId).toBe('42');
  });
});


describe('toolkit MCP sharing persistence', () => {
  it.each([true, false])('sends the explicit sharing value %s on update', async (available) => {
    let saved: unknown;
    server.use(http.put('/api/v2/elitea_core/tool/prompt_lib/2/31', async ({ request }) => {
      saved = await request.json();
      return HttpResponse.json({ id: '31', type: 'openapi', name: 'Echo' });
    }));
    const { result } = renderHookWithProviders(() => useToolkitEdit());
    const meta = { mcp_options: { available_by_mcp: available }, icon_meta: { icon: 'echo' } };
    await result.current({ projectId: '2', toolId: '31', type: 'openapi', meta });
    expect(saved).toEqual({ type: 'openapi', meta });
  });

  it('preserves sharing metadata when creating a toolkit', async () => {
    let saved: unknown;
    server.use(http.post('/api/v2/elitea_core/tools/prompt_lib/2', async ({ request }) => {
      saved = await request.json();
      return HttpResponse.json({ id: '31', type: 'openapi', name: 'Echo' });
    }));
    const { result } = renderHookWithProviders(() => useToolkitCreate());
    const meta = { mcp_options: { available_by_mcp: true } };
    await result.current({ projectId: '2', type: 'openapi', name: 'Echo', meta });
    expect(saved).toEqual({ type: 'openapi', name: 'Echo', meta });
  });

  it('omits metadata when a caller leaves it unchanged', async () => {
    let saved: unknown;
    server.use(http.put('/api/v2/elitea_core/tool/prompt_lib/2/31', async ({ request }) => {
      saved = await request.json();
      return HttpResponse.json({ id: '31', type: 'openapi', name: 'Echo' });
    }));
    const { result } = renderHookWithProviders(() => useToolkitEdit());
    await result.current({ projectId: '2', toolId: '31', type: 'openapi', name: 'Renamed' });
    expect(saved).toEqual({ type: 'openapi', name: 'Renamed' });
  });
});

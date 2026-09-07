import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import type { ApplicationList } from '@/shared/api/generated/model';

import { server } from '../../../test/setup';

import { useOwnerApplications } from './useOwnerApplications';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

const APPLICATION_LIST: ApplicationList = {
  rows: [
    {
      id: 'app-mine',
      name: 'Mine',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      owner_id: 'author-1',
      is_forked: false,
      meta: null,
      has_interrupt: false,
      tags: [],
      status: 'published',
    },
    {
      id: 'app-other',
      name: 'Someone else',
      created_at: '2026-01-02T00:00:00Z',
      updated_at: '2026-01-02T00:00:00Z',
      owner_id: 'author-2',
      is_forked: false,
      meta: null,
      has_interrupt: false,
      tags: [],
      status: 'draft',
    },
  ],
  total: 2,
  page: 1,
  page_size: 20,
  total_pages: 1,
};

describe('useOwnerApplications', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('unwraps the {data, status, headers} envelope eliteaFetch now builds (S4 fix, 2026-07-27)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(APPLICATION_LIST));

    const { result } = renderHook(
      () => useOwnerApplications({ projectId: 'proj-1', authorId: '', statuses: [], forPipeline: false, enabled: true }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    // If `unwrapApplicationList` still read `query.data` as the bare body
    // (pre-fix reality) rather than `query.data.data`, this would resolve
    // to an empty array instead.
    expect(result.current.items.map((i) => i.id)).toEqual(['app-mine', 'app-other']);
  });

  it('filters to the requested author client-side (server does not support author_id)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(APPLICATION_LIST));

    const { result } = renderHook(
      () =>
        useOwnerApplications({ projectId: 'proj-1', authorId: 'author-2', statuses: [], forPipeline: false, enabled: true }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.items.map((i) => i.id)).toEqual(['app-other']);
    expect(result.current.total).toBe(1);
  });

  it('filters by status client-side', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(APPLICATION_LIST));

    const { result } = renderHook(
      () =>
        useOwnerApplications({
          projectId: 'proj-1',
          authorId: '',
          statuses: ['draft'],
          forPipeline: false,
          enabled: true,
        }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.items.map((i) => i.id)).toEqual(['app-other']);
  });

  it('does not fetch when enabled is false', () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(APPLICATION_LIST));

    const { result } = renderHook(
      () => useOwnerApplications({ projectId: 'proj-1', authorId: '', statuses: [], forPipeline: false, enabled: false }),
      { wrapper },
    );

    expect(result.current.items).toEqual([]);
    expect(result.current.isLoading).toBe(false);
  });
});

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithRouter } from '../__tests__/testUtils';

import { useHasApplications } from './useHasApplications';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useHasApplications', () => {
  it('is false, not loading, while there is no selected project', async () => {
    const { result } = renderHookWithRouter(() => useHasApplications());
    await waitFor(() => expect(result.current).toBeDefined());
    expect(result.current.hasApplications).toBe(false);
    expect(result.current.isLoading).toBe(false);
  });

  it('is false when the project has zero applications', async () => {
    server.use(getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }));
    const { result } = renderHookWithRouter(() => useHasApplications(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.hasApplications).toBe(false);
  });

  it('is true when the project has at least one application', async () => {
    server.use(
      getListApplicationsMockHandler({
        rows: [
          {
            id: '1',
            name: 'App',
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
            owner_id: 'user-1',
            is_forked: false,
            meta: null,
            has_interrupt: false,
            tags: [],
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );
    const { result } = renderHookWithRouter(() => useHasApplications(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.hasApplications).toBe(true);
  });
});

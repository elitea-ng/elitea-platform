import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getListToolkitsMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { server } from '@/test/setup';

import { renderHookWithRouter } from '../__tests__/testUtils';

import { useApplicationCatalog } from './useApplicationCatalog';

function application(overrides: Record<string, unknown>) {
  return {
    id: '1',
    name: 'App',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    owner_id: 'user-1',
    is_forked: false,
    meta: null,
    has_interrupt: false,
    tags: [],
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(getListToolkitsMockHandler({}), getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useApplicationCatalog', () => {
  it('is loading with two byRequest entries while there is no selected project', async () => {
    const { result } = renderHookWithRouter(() => useApplicationCatalog());
    // The router's own initial match resolves asynchronously (even against
    // a memory history with no loader), so the probe's first render is not
    // synchronous with `render()` returning — wait for it once, the same
    // way every other test in this file waits for query settlement.
    await waitFor(() => expect(result.current).toBeDefined());
    expect(result.current.applications).toHaveLength(2);
    expect(result.current.applications.every((app) => app.availability === 'byRequest')).toBe(true);
  });

  it('marks a type with a registered application schema as available', async () => {
    server.use(
      getListToolkitsMockHandler({ wikis_Wikis: { metadata: { application: true, label: 'Wikis' } } }),
      getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    );

    const { result } = renderHookWithRouter(() => useApplicationCatalog(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    const wikis = result.current.applications.find((app) => app.type === 'wikis_Wikis');
    expect(wikis?.availability).toBe('available');
    const inventory = result.current.applications.find((app) => app.type === 'inventory');
    expect(inventory?.availability).toBe('byRequest');
  });

  it('marks a type with an existing application instance as configured, matched by either type or agent_type', async () => {
    server.use(
      getListToolkitsMockHandler({}),
      getListApplicationsMockHandler({
        rows: [application({ id: '1', type: 'inventory' }), application({ id: '2', agent_type: 'wikis_Wikis' })],
        total: 2,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );

    const { result } = renderHookWithRouter(() => useApplicationCatalog(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.applications.find((app) => app.type === 'inventory')?.availability).toBe('configured');
    expect(result.current.applications.find((app) => app.type === 'wikis_Wikis')?.availability).toBe('configured');
  });

  it('prefers "configured" over "available" when both a schema and an instance exist', async () => {
    server.use(
      getListToolkitsMockHandler({ inventory: { metadata: { application: true } } }),
      getListApplicationsMockHandler({
        rows: [application({ id: '1', type: 'inventory' })],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );

    const { result } = renderHookWithRouter(() => useApplicationCatalog(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    const inventory = result.current.applications.find((app) => app.type === 'inventory');
    expect(inventory?.availability).toBe('configured');
    expect(inventory?.canCreate).toBe(true);
    expect(inventory?.canRequest).toBe(false);
  });

  it('misses a configured type whose only instance is beyond the first page (documented ListApplicationsParams pagination-contract gap — see this hook\'s own doc comment)', async () => {
    server.use(
      getListToolkitsMockHandler({}),
      // Simulates a project where `inventory`'s one configured instance sits
      // on page 2: `total`/`total_pages` say there is more, but `rows` (all
      // this hook can ever see — `ListApplicationsParams` has no page/limit/
      // offset request field) holds only OTHER applications' rows.
      getListApplicationsMockHandler({
        rows: [application({ id: '1', type: 'other-app' })],
        total: 21,
        page: 1,
        page_size: 20,
        total_pages: 2,
      }),
    );

    const { result } = renderHookWithRouter(() => useApplicationCatalog(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    // This assertion pins the CURRENT, documented limitation rather than the
    // desired end state: it must start failing (and be updated) the day
    // `ListApplicationsParams` gains real pagination and this hook is
    // rewritten to fetch every page.
    const inventory = result.current.applications.find((app) => app.type === 'inventory');
    expect(inventory?.availability).not.toBe('configured');
  });
});

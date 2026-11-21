import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { server } from '@/test/setup';

import { renderHookWithRouter } from '../__tests__/testUtils';

import { useApplicationToolkits } from './useApplicationToolkits';

function toolkit(id: string, type: string, name = `Toolkit ${id}`) {
  return {
    id,
    type,
    name,
    description: '',
    settings: {},
    meta: {},
    created_at: '2026-01-01T00:00:00Z',
    author_id: 1,
  };
}

function toolkitList(...rows: ReturnType<typeof toolkit>[]) {
  return { rows, total: rows.length };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useApplicationToolkits', () => {
  it('keeps only the toolkits of an application type, Wiki and Inventory', async () => {
    server.use(
      getListToolkitInstancesMockHandler(
        toolkitList(toolkit('1', 'github'), toolkit('2', 'wikis_Wikis'), toolkit('3', 'inventory'), toolkit('4', 'jira')),
      ),
    );
    const { result } = renderHookWithRouter(() => useApplicationToolkits(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.applications).toHaveLength(2));
    expect(result.current.applications.map((row) => row.id)).toEqual(['2', '3']);
  });

  it('asks the server for the application types, each spelling as its own type value (#2189)', async () => {
    let requested: URL | undefined;
    server.use(
      http.get('*/elitea_core/tools/prompt_lib/:projectId', ({ request }) => {
        requested = new URL(request.url);
        return HttpResponse.json(toolkitList(toolkit('2', 'wikis_Wikis')));
      }),
    );
    const { result } = renderHookWithRouter(() => useApplicationToolkits(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.applications).toHaveLength(1));
    expect(requested?.searchParams.getAll('type')).toEqual([
      'wikis_Wikis',
      'inventory',
      'deepwiki_Deepwiki',
      'wikis',
      'deepwiki',
    ]);
    expect(requested?.searchParams.get('mcp')).toBe('false');
  });

  it('reports a failed list', async () => {
    server.use(getListToolkitInstancesMockHandler(() => Promise.reject(new Error('boom'))));
    const { result } = renderHookWithRouter(() => useApplicationToolkits(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isError).toBe(true));
  });
});

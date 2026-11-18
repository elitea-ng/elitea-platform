import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { server } from '@/test/setup';

import { renderHookWithRouter } from '../__tests__/testUtils';

import { useHasApplications } from './useHasApplications';

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

describe('useHasApplications', () => {
  it('is false, not loading, while there is no selected project', async () => {
    const { result } = renderHookWithRouter(() => useHasApplications());
    await waitFor(() => expect(result.current).toBeDefined());
    expect(result.current.hasApplications).toBe(false);
    expect(result.current.isLoading).toBe(false);
  });

  it('is false when the project has no toolkit of an application type', async () => {
    server.use(getListToolkitInstancesMockHandler(toolkitList(toolkit('1', 'github'))));
    const { result } = renderHookWithRouter(() => useHasApplications(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.hasApplications).toBe(false);
  });

  // #2113: a created Wiki or Inventory is a toolkit instance, which the applications list never held.
  it('is true when the project has a Wiki or Inventory toolkit', async () => {
    server.use(getListToolkitInstancesMockHandler(toolkitList(toolkit('1', 'github'), toolkit('2', 'inventory'))));
    const { result } = renderHookWithRouter(() => useHasApplications(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.hasApplications).toBe(true));
  });
});

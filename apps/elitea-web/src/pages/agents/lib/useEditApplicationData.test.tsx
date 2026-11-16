import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetApplicationMockHandler,
  getGetApplicationVersionDetailMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useEditApplicationData } from './useEditApplicationData';

function detail() {
  return {
    id: '42',
    name: 'My Agent',
    description: 'A helpful agent',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [
      { id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
      { id: '2', name: 'v2', status: 'published', agent_type: 'classic', created_at: '2026-01-02T00:00:00Z' },
    ],
    version_details: { id: '1', application_id: '42', name: 'base', status: 'draft' },
  };
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useEditApplicationData', () => {
  it('resolves detail/versions/activeVersion from the default (no explicit :version) path', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const { result } = renderHook(() => useEditApplicationData('9', 42, undefined), { wrapper });

    await waitFor(() => expect(result.current.detail?.name).toBe('My Agent'));
    expect(result.current.versions).toHaveLength(2);
    expect(result.current.activeVersion?.id).toBe('1');
  });

  it('does not fetch anything while applicationId is undefined', () => {
    const { result } = renderHook(() => useEditApplicationData('9', undefined, undefined), { wrapper });
    expect(result.current.detail).toBeUndefined();
    expect(result.current.versions).toEqual([]);
  });

  it("fetches the explicit version when the URL :version differs from the detail response's default version", async () => {
    server.use(
      getGetApplicationMockHandler(detail()),
      getGetApplicationVersionDetailMockHandler({
        id: '2',
        application_id: '42',
        name: 'v2',
        status: 'published',
        instructions: 'Explicit version instructions.',
      }),
    );
    const { result } = renderHook(() => useEditApplicationData('9', 42, '2'), { wrapper });

    await waitFor(() => expect(result.current.activeVersion?.id).toBe('2'));
    expect(result.current.activeVersion?.instructions).toBe('Explicit version instructions.');
  });

  it("does NOT issue a second fetch when the URL :version matches the detail response's own default version", async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const { result } = renderHook(() => useEditApplicationData('9', 42, '1'), { wrapper });

    await waitFor(() => expect(result.current.activeVersion?.id).toBe('1'));
    // No crash / no stuck-fetching state — the explicit-version query stays disabled.
    expect(result.current.isFetching).toBe(false);
  });

  it('isDetailNotFound is true when the detail fetch 404s', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    const { result } = renderHook(() => useEditApplicationData('9', 42, undefined), { wrapper });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDetailNotFound).toBe(true);
  });

  it('isDetailNotFound is true when the detail fetch 400s', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'bad request' }, { status: 400 }),
      ),
    );
    const { result } = renderHook(() => useEditApplicationData('9', 42, undefined), { wrapper });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDetailNotFound).toBe(true);
  });

  it('isDetailNotFound stays false for a non-404/400 detail-fetch error (e.g. 500)', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result } = renderHook(() => useEditApplicationData('9', 42, undefined), { wrapper });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDetailNotFound).toBe(false);
  });

  it('isDetailNotFound stays false once the detail fetch succeeds', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const { result } = renderHook(() => useEditApplicationData('9', 42, undefined), { wrapper });

    await waitFor(() => expect(result.current.detail?.name).toBe('My Agent'));
    expect(result.current.isDetailNotFound).toBe(false);
  });
});

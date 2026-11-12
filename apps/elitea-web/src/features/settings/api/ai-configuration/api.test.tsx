/**
 * api.test.tsx — contract coverage for the AI-Configuration model fetcher.
 *
 * DEFECT this file pins:
 *  1. `fetchModels` and `setProjectDefaultModel` called `window.fetch` with a
 *     bare `/configurations/models/...` path. Only the shared HTTP client adds
 *     the `/api/v2` base, so every read and every write answered 404. The read
 *     fell back to `EMPTY_MODELS_RESPONSE`, so each "Default model" select
 *     rendered blank; the write failed with only a `console.error`.
 *  2. `eliteaFetch` resolves to orval's `{data, status, headers}` envelope.
 *     A swap to `eliteaFetch` without unwrapping would leave every field
 *     `undefined` — the same invisible-data class the URL bug produced.
 *  3. A failed save carried only `eliteaFetch: 404 from <url>`, which names no
 *     cause a user can act on. The server's own message is now read from the
 *     error body.
 *
 * The MSW handlers below pin the FULL `/api/v2` path on purpose. A `*`
 * wildcard path matches the un-prefixed URL too, and would let the defect
 * come back unseen.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { EliteaApiError, configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../../test/setup';

import {
  modelConfigurationErrorMessage,
  useModelsQuery,
  useSetProjectDefaultModelMutation,
} from './api';

const BASE = '/api/v2';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

afterEach(() => {
  resetGeneratedClient();
});

describe('useModelsQuery', () => {
  it('requests the /api/v2-based models path and returns the unwrapped body', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    let requestedUrl = '';
    server.use(
      http.get(`${BASE}/configurations/models/7`, ({ request }) => {
        requestedUrl = request.url;
        return HttpResponse.json({
          items: [{ name: 'gpt-4o' }],
          total: 1,
          default_model_name: 'gpt-4o',
          default_model_project_id: '7',
        });
      }),
    );

    const { result } = renderHook(() => useModelsQuery('7', 'llm', true), { wrapper });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(requestedUrl).toContain(`${BASE}/configurations/models/7`);
    expect(requestedUrl).toContain('section=llm');
    expect(requestedUrl).toContain('include_shared=true');
    // The envelope is unwrapped: the real default reaches the caller.
    expect(result.current.data?.default_model_name).toBe('gpt-4o');
    expect(result.current.data?.items).toHaveLength(1);
  });

  it('reports a failed model read as an error instead of an empty list', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/models/7`, () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );

    const { result } = renderHook(() => useModelsQuery('7', 'llm', false), { wrapper });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.error).toBeInstanceOf(EliteaApiError);
  });

  it('keeps include_shared in the cache key so the two variants do not share one entry', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const seen: string[] = [];
    server.use(
      http.get(`${BASE}/configurations/models/7`, ({ request }) => {
        seen.push(new URL(request.url).searchParams.get('include_shared') ?? '');
        return HttpResponse.json({ items: [], total: 0 });
      }),
    );

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    function sharedWrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    }
    const { result } = renderHook(
      ({ includeShared }: { includeShared: boolean }) => useModelsQuery('7', 'llm', includeShared),
      { initialProps: { includeShared: true }, wrapper: sharedWrapper },
    );
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    renderHook(() => useModelsQuery('7', 'llm', false), { wrapper: sharedWrapper });

    await waitFor(() => expect(seen).toContain('false'));
    expect(seen).toContain('true');
  });
});

describe('useSetProjectDefaultModelMutation', () => {
  it('POSTs to the /api/v2-based models path with the section payload', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    let body: unknown;
    let requestedUrl = '';
    server.use(
      http.post(`${BASE}/configurations/models/7`, async ({ request }) => {
        requestedUrl = request.url;
        body = await request.json();
        return HttpResponse.json({ items: [], total: 0 });
      }),
    );

    const { result } = renderHook(() => useSetProjectDefaultModelMutation('7'), { wrapper });
    result.current.mutate({ name: 'gpt-4o', targetProjectId: '7', section: 'llm_high_tier' });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(requestedUrl).toContain(`${BASE}/configurations/models/7`);
    expect(body).toEqual({ name: 'gpt-4o', target_project_id: '7', section: 'llm_high_tier' });
  });

  it('rejects with a typed error the caller can show, instead of only logging', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.post(`${BASE}/configurations/models/7`, () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );

    const { result } = renderHook(() => useSetProjectDefaultModelMutation('7'), { wrapper });
    result.current.mutate({ name: 'gpt-4o', targetProjectId: '7', section: 'llm' });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(modelConfigurationErrorMessage(result.current.error)).toBe('insufficient permissions');
  });
});

describe('modelConfigurationErrorMessage', () => {
  it('prefers the server message field over the generic status line', () => {
    const error = new EliteaApiError({
      kind: 'http',
      status: 400,
      url: `${BASE}/configurations/models/7`,
      body: { message: 'the model is not available' },
    });
    expect(modelConfigurationErrorMessage(error)).toBe('the model is not available');
  });

  it('never shows the eliteaFetch diagnostic for an HTML or empty refusal', () => {
    for (const body of ['<html>502 Bad Gateway</html>', '', null]) {
      const error = new EliteaApiError({ kind: 'http', status: 502, url: `${BASE}/configurations/models/7`, body });
      const message = modelConfigurationErrorMessage(error);
      expect(message).not.toContain('eliteaFetch');
      expect(message).toBe('The default model could not be saved. Try again.');
    }
  });

  it('falls back to a translated sentence for a network error', () => {
    const error = new EliteaApiError({
      kind: 'network',
      url: `${BASE}/configurations/models/7`,
      message: 'http: request failed',
      cause: undefined,
    });
    expect(modelConfigurationErrorMessage(error)).toBe('The default model could not be saved. Try again.');
  });
});

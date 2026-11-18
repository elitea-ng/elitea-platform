import { QueryClient } from '@tanstack/react-query';
import { act, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';

import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';

import { useSetDefaultVersion } from './useSetDefaultVersion';

const SET_DEFAULT_ROUTE = '*/elitea_core/default_version/prompt_lib/:projectId/:applicationId/:versionId';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function baseInput() {
  return { projectId: 'p1', applicationId: 3 };
}

describe('useSetDefaultVersion', () => {
  it('PATCHes the 4-segment default-version route with the version in the PATH and resolves true', async () => {
    const requests: string[] = [];
    server.use(
      http.patch(SET_DEFAULT_ROUTE, ({ request }) => {
        requests.push(new URL(request.url).pathname);
        return HttpResponse.json({ ok: true }, { status: 200 });
      }),
    );
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()));

    let ok;
    await act(async () => {
      ok = await result.current.doSetDefaultVersion(7);
    });

    expect(ok).toBe(true);
    // The baseline PATCHes the 3-SEGMENT form with the id in the body, which
    // the Go router answers with a 405 (router.go:1778 registers only this
    // shape). Asserting the whole path is what tells the two apart.
    expect(requests).toEqual(['/api/v2/elitea_core/default_version/prompt_lib/p1/3/7']);
    await waitFor(() => expect(result.current.isSettingDefaultVersion).toBe(false));
  });

  /**
   * The regression that would otherwise be invisible: the generated client
   * models this PATCH as a QUERY, and the app's own client sets
   * `staleTime: 30_000` (`app/providers/queryClient.ts:101`). Without the
   * hook's `staleTime: 0`, the second call replays the cache entry, sends
   * NOTHING, and still reports success.
   */
  it('sends a request every time, even when the same version is set twice under the app query client staleTime', async () => {
    let calls = 0;
    server.use(
      http.patch(SET_DEFAULT_ROUTE, () => {
        calls += 1;
        return HttpResponse.json({ ok: true }, { status: 200 });
      }),
    );
    const appLikeClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0, staleTime: 30_000 } },
    });
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()), appLikeClient);

    await act(async () => {
      await result.current.doSetDefaultVersion(7);
    });
    await act(async () => {
      await result.current.doSetDefaultVersion(7);
    });

    expect(calls).toBe(2);
  });

  // #1991: the cached application detail embeds the default version; leaving
  // it fresh made reopening the agent replay the old default.
  it('invalidates the cached application detail after a successful pin, and not after a refusal', async () => {
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ ok: true }, { status: 200 })));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 30_000 } } });
    const key = getGetApplicationQueryKey('p1', 3);
    queryClient.setQueryData(key, { data: {} });
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()), queryClient);

    await act(async () => {
      await result.current.doSetDefaultVersion(7);
    });

    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true);
  });

  // The pin succeeded; the cache refresh is housekeeping. Awaiting it let a
  // refetch failure (or a slow one) turn a successful pin into a reported failure.
  it('does not wait for the detail refetch: a pending or failing refresh cannot fail a successful pin', async () => {
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ ok: true }, { status: 200 })));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(queryClient, 'invalidateQueries').mockReturnValue(new Promise<void>(() => undefined));
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()), queryClient);

    let ok;
    await act(async () => {
      ok = await result.current.doSetDefaultVersion(7);
    });

    expect(ok).toBe(true);
    expect(result.current.error).toBeUndefined();
  });

  it('resolves false and reports the server refusal when the version is not this application’s', async () => {
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ error: 'version not found' }, { status: 404 })));
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()));

    let ok;
    await act(async () => {
      ok = await result.current.doSetDefaultVersion(99);
    });

    expect(ok).toBe(false);
    await waitFor(() => expect(result.current.error).toBeDefined());
    expect(result.current.errorMessage?.length).toBeGreaterThan(0);
  });

  it('resetError drops a refusal so the next dialog opening does not re-show it', async () => {
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ error: 'nope' }, { status: 404 })));
    const { result } = renderHookWithProviders(() => useSetDefaultVersion(baseInput()));

    await act(async () => {
      await result.current.doSetDefaultVersion(99);
    });
    await waitFor(() => expect(result.current.errorMessage).toBeDefined());

    act(() => {
      result.current.resetError();
    });

    expect(result.current.errorMessage).toBeUndefined();
  });
});

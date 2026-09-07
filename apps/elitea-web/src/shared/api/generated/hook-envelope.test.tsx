/**
 * hook-envelope.test.tsx — regression coverage for the S4 envelope fix
 * (2026-07-27; see `mutator.ts`'s doc comment and `http.ts`'s `HttpResult`
 * doc comment for the full root-cause account).
 *
 * `mutator.test.ts` proves `eliteaFetch` itself resolves with
 * `{data, status, headers}`. That is necessary but not sufficient: orval's
 * generated `react-query` hooks sit on top of `eliteaFetch`, and it is
 * `query.data`'s shape — not `eliteaFetch`'s — that every Wave-2 feature
 * actually consumes. This test renders a REAL generated hook
 * (`useListApplications`) against MSW and asserts the resolved
 * `query.data` structurally matches its own declared
 * `listApplicationsResponse` type (`{data, status, headers}`), closing the
 * gap a hand-rolled `eliteaFetch<T>()` call can't: proof that nothing
 * between the mutator and `useQuery` (react-query's own unwrapping, the
 * generated `queryFn`) re-strips the envelope back off.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { server } from '../../../test/setup';

import { useListApplications } from './applications/applications';
import { getListApplicationsMockHandler } from './applications/applications.msw';
import type { ApplicationList } from './model';
import { configureGeneratedClient, resetGeneratedClient } from './mutator';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

const APPLICATION_LIST: ApplicationList = {
  rows: [
    {
      id: 'app-1',
      name: 'Envelope probe',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      owner_id: 'author-1',
      is_forked: false,
      meta: null,
      has_interrupt: false,
      tags: [],
      status: 'published',
    },
  ],
  total: 1,
  page: 1,
  page_size: 20,
  total_pages: 1,
};

afterEach(() => {
  resetGeneratedClient();
});

describe('a real generated react-query hook resolves the envelope its own declared type promises', () => {
  it('useListApplications: query.data is {data, status, headers}, and query.data.data is the ApplicationList — not the bare list at the top level', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(APPLICATION_LIST));

    const { result } = renderHook(() => useListApplications('proj-1'), { wrapper });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    const envelope = result.current.data;
    if (envelope === undefined || !('data' in envelope)) {
      throw new Error('unreachable: isSuccess implies a listApplicationsResponseSuccess envelope');
    }

    // The declared type (applications.ts:157-158): {data: ApplicationList,
    // status: 200} & {headers: Headers}. Asserted structurally, not just
    // type-checked, because `tsc --noEmit` alone is exactly what let this
    // bug through the first time — the generic `T` in `eliteaFetch<T>`
    // made the runtime/type mismatch invisible to the compiler.
    expect(envelope.status).toBe(200);
    expect(envelope.headers).toBeInstanceOf(Headers);
    expect(envelope.data).toEqual(APPLICATION_LIST);

    // The failure mode this regression test exists to catch: before the
    // fix, `query.data` WAS the bare `ApplicationList` — `.rows` lived at
    // the top level, not under `.data`. Guard against that shape resurfacing.
    expect((envelope as unknown as Record<string, unknown>).rows).toBeUndefined();
  });
});

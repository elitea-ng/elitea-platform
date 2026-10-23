import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, waitFor } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';

import {
  getCreateModerationRequestMockHandler,
  getModerationStatusMockHandler,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ModerationRequestRow } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { REQUEST_STATUS } from '../lib/constants';

import { useModerationRequests } from './useModerationRequests';
import { renderHookWithRouter } from '../__tests__/testUtils';

/**
 * `../__tests__/testUtils.tsx`'s own `createTestQueryClient()` zeroes out
 * `gcTime` and forces `retry: false` — deliberately, so unrelated tests
 * never depend on cache/retry timing. That also means it CANNOT reproduce
 * the exact regression finding #1 (adversarial-review cluster
 * `A6-api-model`) describes: `submitRequest` routing its POST through
 * `queryClient.query()` against query-flavoured options picks up
 * whatever the *app's real* `defaultOptions.queries` are
 * (`staleTime: 30_000`, `retry: 1` — `src/app/providers/queryClient.ts`'s
 * `QUERY_DEFAULT_OPTIONS`), not the test harness's zeroed ones. Importing
 * that module directly here would pull a `features/` test across the FSD
 * boundary into `app/` (the wrong direction), so the three field values
 * relevant to this regression are duplicated locally instead — this is the
 * "local adapter" the repo's porting rules ask for when a sibling/higher
 * layer's shape is needed but importing it isn't allowed. Keep in sync with
 * `QUERY_DEFAULT_OPTIONS` if that file's `staleTime`/`retry` ever change.
 */
function buildProdParityQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { staleTime: 30_000, retry: 1 },
      mutations: { retry: 0 },
    },
  });
}

/**
 * A full `ModerationRequestRow`, as the spec now models it. The generated msw
 * handlers type their overrides against the real schema. A bare `{status}`
 * does not compile.
 *
 * The return type is the generated type. It keeps every field checked against
 * the schema. Only `status` gets a cast. One test sends a value that is
 * outside the enum. The server can send such a value. The schema is the
 * contract, not a runtime guarantee.
 */
function moderationRow(status: string): ModerationRequestRow {
  return {
    id: 1,
    user_id: 7,
    user_email: 'requester@example.com',
    project_id: 1,
    issue_type: 'Inventory',
    entity_id: 'inventory',
    description: 'I need this for onboarding',
    status: status as ModerationRequestRow['status'],
    rejection_comment: null,
    created_at: '2026-08-01T10:00:00Z',
    updated_at: '2026-08-01T10:00:00Z',
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('entity_id addressing', () => {
  // DEFECT: `v2.yaml` typed the `entity_id` path parameter as an integer.
  // That integer type is a leftover from a retired static stub. The generated
  // client therefore demanded a number, and this hook hashed the catalogue
  // key with FNV-1a. The column is a VARCHAR, the handler stores the raw
  // string, and the legacy UI sends "inventory". One catalogue entry
  // therefore had two addresses.
  //
  // A request filed here was invisible in the other client. That client filed
  // a second row for the same entry. The admin queue showed an opaque
  // number.
  it('addresses the catalogue entry by its raw key, not by a numeric hash', async () => {
    const seen: string[] = [];
    server.use(
      http.get('*/admin/moderation_status/default/:projectId/:entityId', ({ params }) => {
        seen.push(String(params['entityId']));
        return HttpResponse.json({ total: 0, rows: [] });
      }),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(seen).toContain('inventory');
    expect(seen).toContain('wikis_Wikis');
  });

  it('posts a new request to the raw key, with only the two fields a requester owns', async () => {
    let postedTo = '';
    let postedBody: unknown;
    server.use(
      getModerationStatusMockHandler({ total: 0, rows: [] }),
      http.post('*/admin/moderation_status/default/:projectId/:entityId', async ({ params, request }) => {
        postedTo = String(params['entityId']);
        postedBody = await request.json();
        return HttpResponse.json({ id: 1, status: REQUEST_STATUS.PENDING }, { status: 201 });
      }),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isFetching).toBe(false));

    await act(async () => {
      await result.current.submitRequest('inventory', 'I need this for onboarding', 'Inventory');
    });

    expect(postedTo).toBe('inventory');
    // `status` and `meta` are refused or ignored server-side, so they are
    // no longer sent.
    expect(postedBody).toEqual({ issue_type: 'Inventory', description: 'I need this for onboarding' });
  });
});

describe('useModerationRequests', () => {
  it('reports REQUEST_STATUS.NONE for every type while there is no selected project', async () => {
    const { result } = renderHookWithRouter(() => useModerationRequests());
    await waitFor(() => expect(result.current).toBeDefined());
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.NONE);
    expect(result.current.isFetching).toBe(false);
  });

  it('reports REQUEST_STATUS.NONE for a type outside the catalogue', async () => {
    server.use(getModerationStatusMockHandler({ total: 1, rows: [moderationRow(REQUEST_STATUS.APPROVED)] }));
    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.getRequestStatus('not-a-catalog-type')).toBe(REQUEST_STATUS.NONE);
  });

  it('reads the {total, rows} envelope the real endpoint returns, newest row first', async () => {
    // The shape unit A14's Go handler answers with, and the legacy runtime's
    // before it. Until A14 the Go side returned a bare `{"status":"approved"}`
    // from a static stub, and this hook read only that. Against a real server it
    // found no `status` field and fell through to NONE. The "Pending approval"
    // state on a catalogue card was therefore unreachable. The card kept
    // offering "Request Access" after the request had been filed.
    server.use(
      http.get('*/admin/moderation_status/default/:projectId/:entityId', () =>
        HttpResponse.json({
          total: 2,
          rows: [
            { id: 2, status: REQUEST_STATUS.PENDING },
            { id: 1, status: REQUEST_STATUS.REJECTED },
          ],
        }),
      ),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    // `rows[0]`, not `rows[1]`: the server orders newest first and the state the
    // card wants is the most recent request, not the first one ever made.
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.PENDING);
  });

  it('reads a bare {status} body as NONE, not as that status', async () => {
    // The stub's old shape. Only a legacy runtime answered with it, and this
    // hook no longer reads it: a body that is not the envelope is no answer.
    server.use(
      http.get('*/admin/moderation_status/default/:projectId/:entityId', () =>
        HttpResponse.json({ status: REQUEST_STATUS.APPROVED }),
      ),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.NONE);
  });

  it('reports NONE when the envelope carries no rows', async () => {
    // An entity nobody has asked about. The stub answered "approved" here.
    server.use(
      http.get('*/admin/moderation_status/default/:projectId/:entityId', () =>
        HttpResponse.json({ total: 0, rows: [] }),
      ),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.NONE);
  });

  it('reports the status the (stub) backend returns for a real project', async () => {
    server.use(getModerationStatusMockHandler({ total: 1, rows: [moderationRow(REQUEST_STATUS.APPROVED)] }));

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.APPROVED);
    expect(result.current.getRequestStatus('wikis_Wikis')).toBe(REQUEST_STATUS.APPROVED);
  });

  it('falls back to REQUEST_STATUS.NONE for an unrecognised status value from the server', async () => {
    server.use(getModerationStatusMockHandler({ total: 1, rows: [moderationRow('something-unexpected')] }));

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.NONE);
  });

  it('submitRequest sets isSubmitting for the duration of the call and clears it after', async () => {
    server.use(
      getModerationStatusMockHandler({ total: 0, rows: [] }),
      getCreateModerationRequestMockHandler(moderationRow(REQUEST_STATUS.APPROVED)),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), { projectId: 'proj-1' });
    await waitFor(() => expect(result.current.isFetching).toBe(false));

    expect(result.current.isSubmitting).toBe(false);
    await act(async () => {
      await result.current.submitRequest('inventory', 'I need this for onboarding');
    });
    expect(result.current.isSubmitting).toBe(false);
  });

  it('submitRequest is a no-op while there is no selected project', async () => {
    const { result } = renderHookWithRouter(() => useModerationRequests());
    await waitFor(() => expect(result.current).toBeDefined());

    await act(async () => {
      await result.current.submitRequest('inventory', 'reason');
    });
    expect(result.current.getRequestStatus('inventory')).toBe(REQUEST_STATUS.NONE);
  });

  it('submitRequest fires a fresh POST on every call, even with identical arguments inside the real 30s staleTime window (regression test — queryClient.query() against query-flavoured options would dedup this non-idempotent POST against the QueryClient cache and skip the second network call entirely)', async () => {
    let postCount = 0;
    server.use(
      getModerationStatusMockHandler({ total: 0, rows: [] }),
      http.post('*/admin/moderation_status/default/:projectId/:entityId', () => {
        postCount += 1;
        return HttpResponse.json({ status: REQUEST_STATUS.APPROVED });
      }),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), {
      projectId: 'proj-1',
      queryClient: buildProdParityQueryClient(),
    });
    await waitFor(() => expect(result.current.isFetching).toBe(false));

    await act(async () => {
      await result.current.submitRequest('inventory', 'same reason');
    });
    expect(postCount).toBe(1);

    // Same type/description/label as the first call — under the old
    // query()-against-query-options implementation this hashes to the
    // same queryKey and, still inside the 30s staleTime window, resolves
    // from cache with NO second network request.
    await act(async () => {
      await result.current.submitRequest('inventory', 'same reason');
    });
    expect(postCount).toBe(2);
  });

  it('does not auto-retry a failed submitRequest (regression test — queryClient.query() picks up the real query default retry: 1, silently replaying the non-idempotent POST once on failure)', async () => {
    let postCount = 0;
    server.use(
      getModerationStatusMockHandler({ total: 0, rows: [] }),
      http.post('*/admin/moderation_status/default/:projectId/:entityId', () => {
        postCount += 1;
        return HttpResponse.json({ error: 'boom' }, { status: 500 });
      }),
    );

    const { result } = renderHookWithRouter(() => useModerationRequests(), {
      projectId: 'proj-1',
      queryClient: buildProdParityQueryClient(),
    });
    await waitFor(() => expect(result.current.isFetching).toBe(false));

    let caught: unknown;
    await act(async () => {
      try {
        await result.current.submitRequest('inventory', 'reason');
      } catch (error) {
        caught = error;
      }
    });

    expect(caught).toBeDefined();
    expect(postCount).toBe(1);
    expect(result.current.isSubmitting).toBe(false);
  });
});

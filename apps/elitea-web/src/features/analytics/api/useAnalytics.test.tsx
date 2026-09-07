/**
 * Hook-layer coverage for `./useAnalytics.ts` (unit A10; manifest
 * API-008..API-014).
 *
 * The highest-risk logic this unit owns is the parameter mapping for the
 * three detail queries — the decision record's "Analytics detail
 * endpoints" defect fix (send `application_id`/`toolkit_id`/`user_id`, the
 * params the real Go handler's detail branch actually dispatches on, NOT
 * the baseline SPA's `entity_id`/`tool_name`). Every "sends the right
 * query param" test below asserts against the REAL request URL captured
 * off a real `eliteaFetch` → `HttpClient.request` round trip (MSW-mocked
 * at the network boundary only, per R-M1 — no `vi.mock()` of application
 * code anywhere in this file), so a regression that reverts to the
 * baseline's wrong param name fails these tests, not just a unit test of
 * a hand-rolled URL builder.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';
import {
  useAnalyticsAgentDetailQuery,
  useAnalyticsAgentsListQuery,
  useAnalyticsToolDetailQuery,
  useAnalyticsToolsListQuery,
  useAnalyticsUserDetailQuery,
  useAnalyticsUsersListQuery,
  useProjectAnalyticsQuery,
} from './useAnalytics';

const BASE = '/api/v2';
const RANGE = { dateFrom: '2026-07-20T00:00:00.000Z', dateTo: '2026-07-27T00:00:00.000Z' };

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useProjectAnalyticsQuery (API-008)', () => {
  it('resolves the flat ProjectAnalytics payload (not a {data,status,headers} envelope)', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics/prompt_lib/7`, () =>
        HttpResponse.json({
          kpis: {
            unique_users: 3,
            total_project_users: 10,
            ai_active_users: 2,
            adoption_rate: 20,
            llm_calls: 5,
            tool_runs: 1,
            chat_msgs: 4,
            agent_runs: 2,
          },
          top_ai_users: [],
          daily_activity: [],
          models: [],
        }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useProjectAnalyticsQuery('7', RANGE, true), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    // A flat read (`.kpis` directly), not `.data.kpis` — proves the
    // `unwrap()` bridge documented in useAnalytics.ts's header.
    expect(result.current.data?.kpis.llm_calls).toBe(5);
  });

  it('does not fire while projectId is undefined', () => {
    let hit = false;
    server.use(
      http.get(`${BASE}/elitea_core/analytics/prompt_lib/*`, () => {
        hit = true;
        return HttpResponse.json({});
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useProjectAnalyticsQuery(undefined, RANGE, true), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
    expect(hit).toBe(false);
  });

  it('does not fire while the caller-supplied `enabled` is false (Overview/Health tab gating)', () => {
    let hit = false;
    server.use(
      http.get(`${BASE}/elitea_core/analytics/prompt_lib/*`, () => {
        hit = true;
        return HttpResponse.json({});
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useProjectAnalyticsQuery('7', RANGE, false), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
    expect(hit).toBe(false);
  });

  it('sends date_from/date_to as query parameters', async () => {
    let capturedSearch = '';
    server.use(
      http.get(`${BASE}/elitea_core/analytics/prompt_lib/7`, ({ request }) => {
        capturedSearch = new URL(request.url).search;
        return HttpResponse.json({ kpis: {}, top_ai_users: [], daily_activity: [], models: [] });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useProjectAnalyticsQuery('7', RANGE, true), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(capturedSearch).toContain(`date_from=${encodeURIComponent(RANGE.dateFrom)}`);
    expect(capturedSearch).toContain(`date_to=${encodeURIComponent(RANGE.dateTo)}`);
  });
});

describe('useAnalyticsUsersListQuery (API-009)', () => {
  it('resolves the flat {items} list', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_users/prompt_lib/7`, () =>
        HttpResponse.json({ items: [{ user_id: 'u1', email: 'a@x.com', run_count: 3, last_active_at: '2026-07-01T00:00:00Z' }] }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsUsersListQuery('7', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.items).toHaveLength(1);
    expect(result.current.data?.items[0]?.email).toBe('a@x.com');
  });

  it('does not fire while projectId is undefined', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsUsersListQuery(undefined, RANGE), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });
});

describe('useAnalyticsUserDetailQuery (API-010) — the one baseline detail call that already worked', () => {
  it('sends user_id (matches the baseline; the handler already dispatches on it)', async () => {
    let capturedSearch = '';
    server.use(
      http.get(`${BASE}/elitea_core/analytics_user_detail/prompt_lib/7`, ({ request }) => {
        capturedSearch = new URL(request.url).search;
        return HttpResponse.json({ entity_name: 'a@x.com', kpis: {}, agents: [], tools: [], daily_usage: [] });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsUserDetailQuery('7', 'u1', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(capturedSearch).toBe('?user_id=u1');
    // No date_from/date_to — GetAnalyticsUserDetailParams has no such field
    // (see useAnalytics.ts's header: the detail branch never reads them).
    expect(capturedSearch).not.toContain('date_from');
  });

  it('does not fire until both projectId and userId are defined', () => {
    const { wrapper } = createWrapper();
    const { result: noProject } = renderHook(() => useAnalyticsUserDetailQuery(undefined, 'u1', RANGE), {
      wrapper: createWrapper().wrapper,
    });
    expect(noProject.current.fetchStatus).toBe('idle');
    const { result: noUser } = renderHook(() => useAnalyticsUserDetailQuery('7', undefined, RANGE), { wrapper });
    expect(noUser.current.fetchStatus).toBe('idle');
  });
});

describe('useAnalyticsToolsListQuery (API-011)', () => {
  it('resolves the list beside its availability flag', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_tools/prompt_lib/7`, () =>
        HttpResponse.json({
          tool_dimension_available: true,
          items: [
            {
              toolkit_id: 'tk1',
              toolkit_name: 'GitHub',
              tool_name: 'web_search',
              run_count: 9,
              error_count: 0,
              avg_duration_ms: 120,
              error_rate: 0,
            },
          ],
        }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsToolsListQuery('7', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.tool_dimension_available).toBe(true);
    expect(result.current.data?.items?.[0]?.toolkit_id).toBe('tk1');
  });

  // `items` is ABSENT, not empty, for a window the deployment cannot speak for
  // (issue 618). The hook must carry that through rather than normalise it into
  // an empty list, which is what would let the tab render "0 tools".
  it('carries an unavailable window through with no items key', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_tools/prompt_lib/7`, () =>
        HttpResponse.json({ tool_dimension_available: false }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsToolsListQuery('7', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.tool_dimension_available).toBe(false);
    expect(result.current.data?.items).toBeUndefined();
  });
});

describe('useAnalyticsToolDetailQuery (API-012) — the defect fix', () => {
  it('sends toolkit_id, NOT tool_id and NOT the baseline SPA\'s tool_name', async () => {
    let capturedSearch = '';
    server.use(
      http.get(`${BASE}/elitea_core/analytics_tool_detail/prompt_lib/7`, ({ request }) => {
        capturedSearch = new URL(request.url).search;
        return HttpResponse.json({ entity_name: '', kpis: {}, users: [], agents: [], daily_usage: [] });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsToolDetailQuery('7', 'tk1', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(capturedSearch).toBe('?toolkit_id=tk1');
    expect(capturedSearch).not.toContain('tool_id=');
    expect(capturedSearch).not.toContain('tool_name');
  });

  it('does not fire until both projectId and toolkitId are defined', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsToolDetailQuery('7', undefined, RANGE), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });
});

describe('useAnalyticsAgentsListQuery (API-013)', () => {
  it('resolves the flat {items} list', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/analytics_agents/prompt_lib/7`, () =>
        HttpResponse.json({
          items: [{ application_id: 'app1', name: 'My Agent', run_count: 4, avg_duration_ms: 500, total_tokens: 10, error_rate: 0 }],
        }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsAgentsListQuery('7', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.items[0]?.application_id).toBe('app1');
  });
});

describe('useAnalyticsAgentDetailQuery (API-014) — the defect fix', () => {
  it("sends application_id, NOT the baseline SPA's entity_id", async () => {
    let capturedSearch = '';
    server.use(
      http.get(`${BASE}/elitea_core/analytics_agent_detail/prompt_lib/7`, ({ request }) => {
        capturedSearch = new URL(request.url).search;
        return HttpResponse.json({ entity_name: '', kpis: {}, users: [], tools: [], daily_usage: [] });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsAgentDetailQuery('7', 'app1', RANGE), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(capturedSearch).toBe('?application_id=app1');
    expect(capturedSearch).not.toContain('entity_id');
  });

  it('does not fire until both projectId and applicationId are defined', () => {
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAnalyticsAgentDetailQuery(undefined, 'app1', RANGE), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });
});

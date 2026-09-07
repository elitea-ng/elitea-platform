/**
 * useConfigurations.test.tsx — hook-layer coverage for the TanStack Query
 * wrappers over `./configurations.ts` (unit A7). Verifies query enable
 * gating, that a mutation's success invalidates the right cache scope, and
 * that a query actually reaches the network (through the real `eliteaFetch`
 * client, MSW-mocked — no `vi.mock()` of application code, per R-M1).
 * `useTestConfigurationConnection`/`useUpdateConfiguration` get their real
 * exercise in `pages/credentials/CredentialForm.test.tsx` (ACT-041/PUT
 * flow) and `useBatchTestConfigurationConnection` in
 * `model/useCredentialValidation.test.tsx` (ACT-039) — not duplicated here.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { useAvailableConfigurationsType, useConfigurationDetail, useConfigurationsList, useCreateConfiguration, useDeleteConfiguration } from './useConfigurations';

const BASE = '/api/v2';

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode; client: QueryClient } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper, client };
}

afterEach(() => {
  resetGeneratedClient();
});

describe('useConfigurationsList', () => {
  it('fetches and resolves the page envelope', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({ items: [{ type: 'openai' }], total: 1, limit: 20, offset: 0 }),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useConfigurationsList({ projectId: 7 }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.total).toBe(1);
  });

  it('does not fire the request while disabled', () => {
    configureGeneratedClient({ baseUrl: BASE });
    let hit = false;
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () => {
        hit = true;
        return HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useConfigurationsList({ projectId: 7 }, { enabled: false }), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
    expect(hit).toBe(false);
  });
});

describe('useConfigurationDetail', () => {
  it('stays disabled until both projectId and configId are defined', () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useConfigurationDetail(undefined, 'abc'), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });

  /*
   * THE FIRST RENDER OF A DEEP LINK, and the request it used to waste.
   * `routes/-lib/useCredentialFormContext` hands this hook
   * `state.project?.id ?? ''` while the selected-project store is still
   * hydrating, so a blank id is a real, reachable input — not a
   * hypothetical. It used to pass the `!== undefined` guard and fetch
   * `/configurations/configuration//abc`. J19b measured the fallout.
   */
  it('stays disabled while the project id is still the empty string', () => {
    configureGeneratedClient({ baseUrl: BASE });
    let hit = false;
    server.use(
      http.get(`${BASE}/configurations/configuration/:projectId/abc`, () => {
        hit = true;
        return HttpResponse.json({ uid: 'abc' });
      }),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useConfigurationDetail('', 'abc'), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
    expect(hit).toBe(false);
  });

  it('fetches once both ids are present', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({ uid: 'abc', type: 'openai' })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useConfigurationDetail(7, 'abc'), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.uid).toBe('abc');
  });
});

describe('useAvailableConfigurationsType', () => {
  it('resolves the type list', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () =>
        HttpResponse.json([{ type: 'openai', config_schema: { properties: {} } }]),
      ),
    );
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAvailableConfigurationsType({ section: 'llm' }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
  });

  it('respects options.enabled', () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useAvailableConfigurationsType({}, { enabled: false }), { wrapper });
    expect(result.current.fetchStatus).toBe('idle');
  });
});

describe('useCreateConfiguration', () => {
  it('invalidates the configurations + models query scope on success', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ uid: 'new-1', type: 'openai' })),
    );
    const { wrapper, client } = createWrapper();
    const invalidated: unknown[] = [];
    const originalInvalidate = client.invalidateQueries.bind(client);
    client.invalidateQueries = (filters?: Parameters<typeof originalInvalidate>[0]) => {
      invalidated.push(filters?.queryKey);
      return originalInvalidate(filters);
    };

    const { result } = renderHook(() => useCreateConfiguration(), { wrapper });
    result.current.mutate({ projectId: 7, body: { elitea_title: 'a', type: 'openai', data: {} } });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(invalidated).toContainEqual(['credentials', 'configurations']);
    expect(invalidated).toContainEqual(['credentials', 'models']);
  });

  /**
   * #131: the AI-Configuration screen reads the same server resource under
   * `['settings', 'configurations', ...]` (useConfigurationsBySection) and
   * `['models', ...]` (shared/api/configurationsApi). Invalidating only the
   * `credentials` roots left those caches untouched, so after saving a
   * credential the app navigated back to that screen and fired NO list
   * request — the new row stayed invisible until a full page reload.
   */
  it('also invalidates the settings namespace the AI-Configuration screen reads', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ uid: 'new-1', type: 'openai' })),
    );
    const { wrapper, client } = createWrapper();
    const invalidated: unknown[] = [];
    const originalInvalidate = client.invalidateQueries.bind(client);
    client.invalidateQueries = (filters?: Parameters<typeof originalInvalidate>[0]) => {
      invalidated.push(filters?.queryKey);
      return originalInvalidate(filters);
    };

    const { result } = renderHook(() => useCreateConfiguration(), { wrapper });
    result.current.mutate({ projectId: 7, body: { elitea_title: 'a', type: 'openai', data: {} } });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(invalidated).toContainEqual(['settings', 'configurations']);
    expect(invalidated).toContainEqual(['settings', 'availableTypes']);
    expect(invalidated).toContainEqual(['models']);
  });
});

describe('useDeleteConfiguration', () => {
  it('resolves and invalidates on success', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.delete(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({})));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useDeleteConfiguration(), { wrapper });
    result.current.mutate({ projectId: 7, configId: 'abc' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
  });

  // Removal has the same two-namespace problem as creation (#131): the list
  // the user is returned to must refetch, not keep showing the deleted row.
  it('invalidates the settings namespace so the deleted row leaves the list', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.delete(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({})));
    const { wrapper, client } = createWrapper();
    const invalidated: unknown[] = [];
    const originalInvalidate = client.invalidateQueries.bind(client);
    client.invalidateQueries = (filters?: Parameters<typeof originalInvalidate>[0]) => {
      invalidated.push(filters?.queryKey);
      return originalInvalidate(filters);
    };

    const { result } = renderHook(() => useDeleteConfiguration(), { wrapper });
    result.current.mutate({ projectId: 7, configId: 'abc' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(invalidated).toContainEqual(['credentials', 'configurations']);
    expect(invalidated).toContainEqual(['settings', 'configurations']);
    expect(invalidated).toContainEqual(['models']);
  });
});

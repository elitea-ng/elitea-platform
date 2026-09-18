/**
 * #937 — the read half. Asserts the URL actually asked for (shared rows
 * included, credentials section) and the no-verdict contract, both of which
 * decide whether a warning can appear at all.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { buildConfigurationsListUrl } from './configurations';
import { useProjectCredentialTitles } from './useProjectCredentialTitles';

function wrapper({ children }: { readonly children: React.ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('buildConfigurationsListUrl', () => {
  it('asks for the credentials section INCLUDING shared rows — a credential shared into the project resolves too', () => {
    const url = buildConfigurationsListUrl('proj-1', 'credentials');
    expect(url).toContain('/configurations/configurations/proj-1?');
    expect(url).toContain('include_shared=true');
    expect(url).toContain('section=credentials');
  });
});

describe('useProjectCredentialTitles', () => {
  it('collects the titles of the project rows AND the shared rows', async () => {
    server.use(
      http.get('/api/v2/configurations/configurations/:projectId', () =>
        HttpResponse.json({ items: [{ type: 'github', elitea_title: 'own' }], total: 1, shared: { items: [{ type: 'jira', elitea_title: 'borrowed' }], total: 1 } }),
      ),
    );
    const { result } = renderHook(() => useProjectCredentialTitles('proj-1'), { wrapper });
    await waitFor(() => {
      expect(result.current).toBeDefined();
    });
    expect([...(result.current ?? [])].sort()).toEqual(['borrowed', 'own']);
  });

  it('answers undefined — NO VERDICT — when no project is named, so nothing is asked', () => {
    const { result } = renderHook(() => useProjectCredentialTitles(undefined), { wrapper });
    expect(result.current).toBeUndefined();
  });
});

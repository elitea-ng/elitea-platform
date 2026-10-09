import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useToolkitTools } from './toolkitToolsApi';

const AVAILABLE_TOOLS_URL = '/api/v2/elitea_core/toolkit_available_tools/prompt_lib/proj-1/tk-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderTools() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { readonly children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  return renderHook(() => useToolkitTools({ projectId: 'proj-1', toolkitId: 'tk-1' }), { wrapper });
}

describe('useToolkitTools discovery-off classification', () => {
  it('reports the fixed 503 body as discovery turned off', async () => {
    server.use(http.get(AVAILABLE_TOOLS_URL, () => HttpResponse.json({ error: 'toolkit discovery unavailable' }, { status: 503 })));

    const { result } = renderTools();

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDiscoveryDisabled).toBe(true);
  });

  it('keeps a 503 with another body as an ordinary failure', async () => {
    server.use(http.get(AVAILABLE_TOOLS_URL, () => HttpResponse.json({ error: 'toolkit settings could not be resolved' }, { status: 503 })));

    const { result } = renderTools();

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDiscoveryDisabled).toBe(false);
  });

  it('keeps a 500 with the discovery body as an ordinary failure', async () => {
    server.use(http.get(AVAILABLE_TOOLS_URL, () => HttpResponse.json({ error: 'toolkit discovery unavailable' }, { status: 500 })));

    const { result } = renderTools();

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDiscoveryDisabled).toBe(false);
  });

  it('keeps a 503 with no JSON body as an ordinary failure', async () => {
    server.use(http.get(AVAILABLE_TOOLS_URL, () => new HttpResponse('Service Unavailable', { status: 503 })));

    const { result } = renderTools();

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.isDiscoveryDisabled).toBe(false);
  });

  it('is not set after a successful read', async () => {
    server.use(http.get(AVAILABLE_TOOLS_URL, () => HttpResponse.json({ tools: [{ id: '1', name: 'alpha', type: 'github' }], total: 1 })));

    const { result } = renderTools();

    await waitFor(() => expect(result.current.toolNames).toEqual(['alpha']));
    expect(result.current.isError).toBe(false);
    expect(result.current.isDiscoveryDisabled).toBe(false);
  });
});

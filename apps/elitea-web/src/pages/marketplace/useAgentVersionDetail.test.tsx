import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { getGetPublicApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../test/setup';

import { useAgentVersionDetail } from './useAgentVersionDetail';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

describe('useAgentVersionDetail (adversarial-review fix, cluster A13-agents-hub, finding 2)', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('fetches the version detail for the given applicationId/versionName and exposes welcome_message + conversation_starters', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let requestedPath: string | undefined;
    server.use(
      getGetPublicApplicationMockHandler(info => {
        requestedPath = new URL(info.request.url).pathname;
        return {
          id: '42',
          name: 'Research Agent',
          description: '',
          version_details: {
            id: 'v-1',
            application_id: '42',
            name: 'v1',
            status: 'published',
            welcome_message: 'Hello!',
            conversation_starters: ['Find a paper'],
          },
        };
      }),
    );

    const { result } = renderHook(() => useAgentVersionDetail('42', 'v1'), { wrapper });

    await waitFor(() => expect(result.current.versionDetails?.welcome_message).toBe('Hello!'));
    expect(result.current.versionDetails?.conversation_starters).toEqual(['Find a paper']);
    expect(requestedPath).toBe('/api/v2/elitea_core/public_application/prompt_lib/42/v1');
  });

  it('does not fetch when versionName is empty', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let called = false;
    server.use(
      getGetPublicApplicationMockHandler(() => {
        called = true;
        return {
          id: '42',
          name: 'Research Agent',
          description: '',
          version_details: { id: 'v-1', application_id: '42', name: 'v1', status: 'published' },
        };
      }),
    );

    const { result } = renderHook(() => useAgentVersionDetail('42', ''), { wrapper });

    await new Promise(resolve => setTimeout(resolve, 10));
    expect(called).toBe(false);
    expect(result.current.versionDetails).toBeUndefined();
  });
});

/**
 * #1972 — an agent opened from the participant rail got a header over an empty
 * body. `useChatAgentConfig` is what fills it: a form seeded from the stored
 * version, and a Save that writes it back without dropping the stored `meta`.
 */
import type { ReactElement, ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetApplicationMockHandler, getGetApplicationVersionDetailMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useChatAgentConfig } from './useChatAgentConfig';

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

const PARTICIPANT = { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } };

describe('useChatAgentConfig', () => {
  it('offers no save while disabled', () => {
    const { result } = renderHook(() => useChatAgentConfig({ projectId: '7', participant: PARTICIPANT, enabled: false }), { wrapper });
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('renders the instructions form seeded from the stored version and saves without dropping stored meta', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({
        id: '10',
        application_id: '1',
        name: 'latest',
        status: 'draft',
        instructions: 'Be clear',
        meta: { notes: 'keep me', step_limit: 7 },
      }),
      getGetApplicationMockHandler({ id: '1', name: 'Clarifier', description: 'd', icon: '', owner_id: 'u', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    let body: Record<string, unknown> | undefined;
    server.use(
      http.put('/api/v2/elitea_core/version/prompt_lib/7/1/10', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: '10', application_id: '1', name: 'latest', status: 'draft' });
      }),
      http.put('/api/v2/elitea_core/application/prompt_lib/7/1', () => HttpResponse.json({ id: '1', name: 'Clarifier' })),
    );
    const { result } = renderHook(() => useChatAgentConfig({ projectId: '7', participant: PARTICIPANT, enabled: true }), { wrapper });

    await waitFor(() => expect(result.current.onSaveVersion).toBeDefined());
    await waitFor(() => {
      const form = result.current.renderConfigurationForm() as ReactElement<{ values: { name: string } }>;
      expect(form.props.values.name).toBe('Clarifier');
    });
    const form = result.current.renderConfigurationForm() as ReactElement<{
      showInstructions: boolean;
      entityType: string;
      values: { version_details: { instructions: string } };
    }>;
    expect(form.props.showInstructions).toBe(true);
    expect(form.props.entityType).toBe('application');
    expect(form.props.values.version_details.instructions).toBe('Be clear');

    await act(async () => {
      result.current.onSaveVersion?.();
    });
    await waitFor(() => expect(body).toBeDefined());
    expect(body?.['instructions']).toBe('Be clear');
    expect(body?.['meta']).toMatchObject({ notes: 'keep me', step_limit: 7 });
  });
});

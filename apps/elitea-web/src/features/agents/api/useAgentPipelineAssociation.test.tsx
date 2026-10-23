import type { ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { getGetApplicationMockHandler, getUpdateApplicationRelationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useAgentPipelineAssociation } from './useAgentPipelineAssociation';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function detail(overrides: Record<string, unknown> = {}) {
  return {
    id: '42',
    name: 'Helper Bot',
    description: '',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [],
    version_details: {
      id: '100',
      application_id: '42',
      name: 'base',
      status: 'draft',
      agent_type: undefined,
      tools: [],
      meta: {},
    },
    ...overrides,
  };
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return (
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        {children}
      </ThemeProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useAgentPipelineAssociation', () => {
  it('reports a validation message and makes no request when ids are missing', async () => {
    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: undefined, applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() => result.current.associateAgent({ id: 42, name: 'Helper Bot' }));
    expect(outcome).toEqual({ ok: false, message: 'Application ID and Version ID are required to associate agent' });
  });

  it('successfully associates a plain agent', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    server.use(getUpdateApplicationRelationMockHandler({ application_id: '1', version_id: '2', has_relation: true }));

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() => result.current.associateAgent({ id: 42, name: 'Helper Bot' }));

    expect(outcome).toEqual({ ok: true, message: 'The "Helper Bot" agent is successfully added.' });
  });

  it('attaches a sub-orchestrator (an agent that uses other agents): the 3-level rule is the server\'s (FU-244)', async () => {
    server.use(
      getGetApplicationMockHandler(
        detail({
          version_details: {
            id: '100',
            application_id: '42',
            name: 'base',
            status: 'draft',
            agent_type: undefined,
            tools: [{ id: 1, type: 'application' }],
            meta: {},
          },
        }),
      ),
    );
    server.use(getUpdateApplicationRelationMockHandler({ application_id: '1', version_id: '2', has_relation: true }));

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() => result.current.associateAgent({ id: 42, name: 'Research Lead' }));

    expect(outcome).toEqual({ ok: true, message: 'The "Research Lead" agent is successfully added.' });
  });

  it('allows a pipeline candidate to contain application-type tools', async () => {
    server.use(
      getGetApplicationMockHandler(
        detail({
          version_details: {
            id: '100',
            application_id: '42',
            name: 'base',
            status: 'draft',
            agent_type: 'pipeline',
            tools: [{ id: 1, type: 'application' }],
            meta: {},
          },
        }),
      ),
    );
    server.use(getUpdateApplicationRelationMockHandler({ application_id: '1', version_id: '2', has_relation: true }));

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() => result.current.associateAgent({ id: 42, name: 'Flow' }, { isPipeline: true }));

    expect(outcome).toEqual({ ok: true, message: 'The "Flow" pipeline is successfully added.' });
  });

  it('rejects a duplicate association client-side without a network call', async () => {
    server.use(getGetApplicationMockHandler(detail()));

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() =>
      result.current.associateAgent(
        { id: 42, name: 'Helper Bot' },
        { currentTools: [{ type: 'application', settings: { application_id: '42' } }] },
      ),
    );

    expect(outcome).toEqual({ ok: false, message: 'The "Helper Bot" agent is already added to this agent.' });
  });

  it('maps a circular-reference backend rejection through mapAssociationError', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    // A real 400 error body, matching applications.ts's documented `{"error": "..."}` shape —
    // driven directly via msw's http/HttpResponse rather than the generated mock helper
    // (which only ever emits the 201 success shape).
    const { http, HttpResponse } = await import('msw');
    server.use(
      http.patch('*/elitea_core/application_relation/prompt_lib/:projectId/:selectedApplicationId/:selectedVersionId', () =>
        HttpResponse.json({ error: 'circular reference detected' }, { status: 400 }),
      ),
    );

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    const outcome = await act(() => result.current.associateAgent({ id: 42, name: 'Helper Bot' }));
    expect(outcome.ok).toBe(false);
    expect(outcome.message).toContain('circular agent reference');
  });

  it('isAssociating flips true then false around a call', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    server.use(getUpdateApplicationRelationMockHandler({ application_id: '1', version_id: '2', has_relation: true }));

    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    expect(result.current.isAssociating).toBe(false);
    await act(() => result.current.associateAgent({ id: 42, name: 'Helper Bot' }));
    expect(result.current.isAssociating).toBe(false);
  });
});

describe('useAgentPipelineAssociation getToolIcon', () => {
  it('returns an ApplicationsIcon element for "agent"', () => {
    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    expect(result.current.getToolIcon('agent')).not.toBeNull();
  });

  it('returns a FlowIcon element for "pipeline"', () => {
    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    expect(result.current.getToolIcon('pipeline')).not.toBeNull();
  });

  it('returns null for any other type', () => {
    const { result } = renderHook(() => useAgentPipelineAssociation({ projectId: 'proj-1', applicationId: 1, versionId: 2 }), { wrapper });
    expect(result.current.getToolIcon('github')).toBeNull();
    expect(result.current.getToolIcon(undefined)).toBeNull();
  });
});

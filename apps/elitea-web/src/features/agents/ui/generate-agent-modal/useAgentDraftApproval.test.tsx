import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  getCreateApplicationMockHandler,
  getGetApplicationMockHandler,
  getUpdateApplicationRelationMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { EMPTY_AGENT_DRAFT, type AgentDraft } from '../../lib/agentDraft';
import { useAgentDraftApproval } from './useAgentDraftApproval';

function createWrapper(): ({ children }: { children: ReactNode }) => ReactNode {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useAgentDraftApproval', () => {
  it('creates the application and returns the server response', async () => {
    server.use(
      getCreateApplicationMockHandler({
        id: '42',
        name: 'My Agent',
        description: 'desc',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
        version_details: { id: '7', application_id: '42', name: 'base', status: 'draft' },
      }),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });

    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, name: 'My Agent', description: 'desc' };
    let response;
    await act(async () => {
      response = await result.current.approve(draft, { selectedAgentIds: new Set(), selectedPipelineIds: new Set() });
    });

    expect(response).toEqual(expect.objectContaining({ id: '42', name: 'My Agent' }));
    expect(result.current.isApproving).toBe(false);
  });

  it("sends the reviewed persona as the new version's soul, and none when it was removed (issue 1644)", async () => {
    const bodies: unknown[] = [];
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        bodies.push(await info.request.json());
        return { id: '42', name: 'A', description: '', type: 'interface', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z' };
      }),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });
    const soul = { schema: 'elitea.soul/v1', display_name: 'Mira', role: 'Incident lead' };
    const selection = { selectedAgentIds: new Set<number | string>(), selectedPipelineIds: new Set<number | string>() };
    await act(async () => {
      await result.current.approve({ ...EMPTY_AGENT_DRAFT, name: 'A', soul }, selection);
      await result.current.approve({ ...EMPTY_AGENT_DRAFT, name: 'A' }, selection);
    });
    expect((bodies[0] as { versions: [{ soul?: unknown }] }).versions[0].soul).toEqual(soul);
    expect((bodies[1] as { versions: [{ soul?: unknown }] }).versions[0]).not.toHaveProperty('soul');
  });

  it('sends the trimmed name and mapped version fields on create', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'My Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });

    const draft: AgentDraft = {
      ...EMPTY_AGENT_DRAFT,
      name: '  My Agent  ',
      instructions: 'Use {{topic}} carefully',
      conversation_starters: ['Hi', ''],
    };
    await act(async () => {
      await result.current.approve(draft, { selectedAgentIds: new Set(), selectedPipelineIds: new Set() });
    });

    expect(capturedBody).toMatchObject({
      name: 'My Agent',
      versions: [
        expect.objectContaining({
          instructions: 'Use {{topic}} carefully',
          conversation_starters: ['Hi'],
          variables: [{ name: 'topic', value: '' }],
        }),
      ],
    });
    // agent_type is omitted entirely (defaults to "openai" server-side) — see the module doc comment.
    expect((capturedBody as { versions: [{ agent_type?: string }] }).versions[0].agent_type).toBeUndefined();
  });

  it('associates a selected suggested agent via updateApplicationRelation', async () => {
    server.use(
      getCreateApplicationMockHandler({
        id: '42',
        name: 'My Agent',
        description: '',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
        version_details: { id: '7', application_id: '42', name: 'base', status: 'draft' },
      }),
      getGetApplicationMockHandler({
        id: '10',
        name: 'Sub Agent',
        description: '',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
        versions: [],
        version_details: { id: '11', application_id: '10', name: 'base', status: 'draft' },
      }),
    );
    let relationCall: { params: unknown; body: unknown } | undefined;
    server.use(
      getUpdateApplicationRelationMockHandler(async (info) => {
        relationCall = { params: info.params, body: await info.request.json() };
        return { application_id: '42', version_id: '7', has_relation: true };
      }),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });

    const draft: AgentDraft = {
      ...EMPTY_AGENT_DRAFT,
      suggested_agents: [{ id: 10, name: 'Sub Agent' }],
    };
    await act(async () => {
      await result.current.approve(draft, { selectedAgentIds: new Set([10]), selectedPipelineIds: new Set() });
    });

    expect(relationCall?.body).toEqual({ application_id: 42, version_id: 7, has_relation: true });
  });

  it('warns (does not throw) when a selected association fails', async () => {
    server.use(
      getCreateApplicationMockHandler({
        id: '42',
        name: 'My Agent',
        description: '',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
        version_details: { id: '7', application_id: '42', name: 'base', status: 'draft' },
      }),
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    const onAssociationWarning = vi.fn();
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1', onAssociationWarning }), {
      wrapper: createWrapper(),
    });

    const draft: AgentDraft = {
      ...EMPTY_AGENT_DRAFT,
      suggested_agents: [{ id: 10, name: 'Sub Agent' }],
    };
    await act(async () => {
      await result.current.approve(draft, { selectedAgentIds: new Set([10]), selectedPipelineIds: new Set() });
    });

    // `mapAssociationError` only prefixes/names the entity for RECOGNISED substrings
    // (circular/"uses other agents"/bind-itself — see that file's own doc comment) —
    // `applicationErrorMessage` (the real, generic `EliteaApiError.message` adapter this
    // hook feeds it) never produces one of those substrings, so the unrecognised branch
    // returns the generic message verbatim, with no entity name. Real, observed behaviour,
    // not a fabricated expectation.
    expect(onAssociationWarning).toHaveBeenCalledTimes(1);
    expect(onAssociationWarning.mock.calls[0]?.[0]).toContain('eliteaFetch: 404');
  });

  it('falls back to console.warn when onAssociationWarning is not supplied', async () => {
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    server.use(
      getCreateApplicationMockHandler({
        id: '42',
        name: 'My Agent',
        description: '',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
        version_details: { id: '7', application_id: '42', name: 'base', status: 'draft' },
      }),
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });

    const draft: AgentDraft = { ...EMPTY_AGENT_DRAFT, suggested_agents: [{ id: 10, name: 'Sub Agent' }] };
    await act(async () => {
      await result.current.approve(draft, { selectedAgentIds: new Set([10]), selectedPipelineIds: new Set() });
    });

    expect(consoleWarn).toHaveBeenCalled();
    consoleWarn.mockRestore();
  });

  it('throws when create fails to produce a result', async () => {
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result } = renderHook(() => useAgentDraftApproval({ projectId: 'p1' }), { wrapper: createWrapper() });

    await expect(
      result.current.approve(EMPTY_AGENT_DRAFT, { selectedAgentIds: new Set(), selectedPipelineIds: new Set() }),
    ).rejects.toThrow('Failed to create the agent.');
    expect(result.current.isApproving).toBe(false);
  });
});

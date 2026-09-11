import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getCreateApplicationMockHandler,
  getUpdateApplicationVersionMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useCreateApplicationInitialValues } from './initialValues';
import { useCreateApplicationDraft, useSaveApplicationVersion } from './mutations';

function createWrapper(): ({ children }: { children: ReactNode }) => ReactNode {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
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

describe('useCreateApplicationDraft', () => {
  it('is a no-op and returns undefined while projectId is undefined', async () => {
    const { result } = renderHook(() => useCreateApplicationDraft(undefined), {
      wrapper: createWrapper(),
    });
    await act(async () => {
      const response = await result.current.create({ name: 'Agent' });
      expect(response).toBeUndefined();
    });
    expect(result.current.error).toBeUndefined();
  });

  it('creates an application and returns the server response', async () => {
    server.use(
      getCreateApplicationMockHandler({
        id: '42',
        name: 'Agent',
        description: 'Does things',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
      }),
    );
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    let response;
    await act(async () => {
      response = await result.current.create({
        name: 'Agent',
        description: 'Does things',
      });
    });

    expect(response).toEqual(
      expect.objectContaining({
        id: '42',
        name: 'Agent',
        description: 'Does things',
      }),
    );
    expect(result.current.isCreating).toBe(false);
    expect(result.current.error).toBeUndefined();
  });

  it('sends the version draft when one is supplied', async () => {
    let capturedBody: unknown;
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = await info.request.json();
        return {
          id: '42',
          name: 'Pipeline',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result: draftResult } = renderHook(() => useCreateApplicationInitialValues(true));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.create({
        name: 'Pipeline',
        version: { ...draftResult.current.versionDetails, tags: ['mcp'] },
      });
    });

    expect(capturedBody).toMatchObject({
      name: 'Pipeline',
      versions: [
        expect.objectContaining({
          agent_type: 'pipeline',
          name: 'base',
          tags: [{ name: 'mcp' }],
        }),
      ],
    });
  });

  // The three assertions that decide whether an author's model choice reaches
  // the database at all. `model_project_id` is checked on the RAW body text
  // because the worker's `positive_u32` hard-fails on a string and a
  // pretty-printed object hides the difference.
  it('sends llm_settings when the draft carries one, with model_project_id as a number', async () => {
    let capturedText = '';
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedText = await info.request.text();
        return {
          id: '42',
          name: 'Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result: draftResult } = renderHook(() => useCreateApplicationInitialValues(false));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.create({
        name: 'Agent',
        version: {
          ...draftResult.current.versionDetails,
          llmSettings: {
            model_name: 'qwen3.5',
            model_project_id: 17,
            max_tokens: -1,
            temperature: 0.6,
          },
        },
      });
    });

    const body = JSON.parse(capturedText) as {
      versions: { llm_settings?: unknown }[];
    };
    expect(body.versions[0]?.llm_settings).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
    expect(capturedText).toContain('"model_project_id":17');
    expect(capturedText).not.toContain('"model_project_id":"17"');
  });

  // Absent, not `undefined`. `UpdateVersion`'s repository only adds
  // `llm_settings` to its SET list when the decoded value is non-nil
  // (`internal/infra/db/repos/applications.go`), and on create the missing key
  // is what leaves the platform's catalogue-default fallback in charge.
  it('omits the llm_settings key entirely when the draft has none', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = (await info.request.json()) as Record<string, unknown>;
        return {
          id: '42',
          name: 'Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result: draftResult } = renderHook(() => useCreateApplicationInitialValues(false));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.create({
        name: 'Agent',
        version: draftResult.current.versionDetails,
      });
    });

    const version = (capturedBody['versions'] as Record<string, unknown>[])[0] ?? {};
    expect(Object.hasOwn(version, 'llm_settings')).toBe(false);
  });

  /*
   * The create half of the welcome-message defect: `toVersionWriteRequest`
   * had no `welcome_message` branch at all, so the key never reached the
   * POST body no matter what the page held. Proven red before the fix —
   * this assertion read `expected undefined to be 'Hi there'`.
   */
  it('sends welcome_message when the draft carries one', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = (await info.request.json()) as Record<string, unknown>;
        return {
          id: '42',
          name: 'Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result: draftResult } = renderHook(() => useCreateApplicationInitialValues(false));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.create({
        name: 'Agent',
        version: {
          ...draftResult.current.versionDetails,
          welcomeMessage: 'Hi there',
        },
      });
    });

    const version = (capturedBody['versions'] as Record<string, unknown>[])[0] ?? {};
    expect(version['welcome_message']).toBe('Hi there');
  });

  // Absent, not `undefined` — the same omit-don't-blank rule `llm_settings`
  // and `pipeline_settings` follow, so a caller that carries no welcome
  // message cannot clear a stored one.
  it('omits the welcome_message key entirely when the draft names none', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      getCreateApplicationMockHandler(async (info) => {
        capturedBody = (await info.request.json()) as Record<string, unknown>;
        return {
          id: '42',
          name: 'Agent',
          description: '',
          type: 'interface',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
        };
      }),
    );
    const { result: draftResult } = renderHook(() => useCreateApplicationInitialValues(false));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.create({
        name: 'Agent',
        version: draftResult.current.versionDetails,
      });
    });

    const version = (capturedBody['versions'] as Record<string, unknown>[])[0] ?? {};
    expect(Object.hasOwn(version, 'welcome_message')).toBe(false);
  });

  it('captures an error and clears isCreating on failure', async () => {
    server.use(http.post('*/elitea_core/applications/prompt_lib/:projectId', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const { result } = renderHook(() => useCreateApplicationDraft('p1'), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      const response = await result.current.create({ name: 'Agent' });
      expect(response).toBeUndefined();
    });

    expect(result.current.error).toBeDefined();
    await waitFor(() => expect(result.current.isCreating).toBe(false));
  });
});

describe('useSaveApplicationVersion', () => {
  it('is a no-op and returns undefined while any id is undefined', async () => {
    const { result } = renderHook(() => useSaveApplicationVersion(undefined, undefined, undefined), {
      wrapper: createWrapper(),
    });
    await act(async () => {
      const response = await result.current.save({
        name: 'base',
        agentType: undefined,
        instructions: '',
        conversationStarters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        llmSettings: undefined,
        tags: [],
        tools: [],
        pipelineSettings: undefined,
      });
      expect(response).toBeUndefined();
    });
  });

  it('saves a version and returns the server response', async () => {
    server.use(
      getUpdateApplicationVersionMockHandler({
        id: '7',
        application_id: '1',
        name: 'base',
        status: 'draft',
      }),
    );
    const { result } = renderHook(() => useSaveApplicationVersion('p1', 1, 7), {
      wrapper: createWrapper(),
    });

    let response;
    await act(async () => {
      response = await result.current.save({
        name: 'base',
        agentType: undefined,
        instructions: 'Do the thing',
        conversationStarters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        llmSettings: undefined,
        tags: [],
        tools: [],
        pipelineSettings: undefined,
      });
    });

    expect(response).toEqual(expect.objectContaining({ id: '7', application_id: '1', name: 'base' }));
    expect(result.current.isSaving).toBe(false);
    expect(result.current.error).toBeUndefined();
  });

  it('replaces the stored version tags by name, including an empty list', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        bodies.push((await info.request.json()) as Record<string, unknown>);
        return { id: '7', application_id: '1', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useSaveApplicationVersion('p1', 1, 7), {
      wrapper: createWrapper(),
    });
    const baseDraft = {
      name: 'base',
      agentType: 'pipeline' as const,
      instructions: '',
      conversationStarters: [],
      variables: [],
      meta: { step_limit: 25, internal_tools: [] },
      llmSettings: undefined,
      tools: [],
      pipelineSettings: undefined,
    };

    await act(async () => {
      await result.current.save({ ...baseDraft, tags: ['mcp', 'release'] });
      await result.current.save({ ...baseDraft, tags: [] });
    });

    expect(bodies[0]?.['tags']).toEqual([{ name: 'mcp' }, { name: 'release' }]);
    expect(bodies[1]?.['tags']).toEqual([]);
  });

  // #135: the PUT used to leave the flow graph off the wire entirely, so the
  // server answered 200 and the edit was gone on reload.
  it('puts the draft pipelineSettings on the wire as pipeline_settings', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        capturedBody = (await info.request.json()) as Record<string, unknown>;
        return { id: '7', application_id: '1', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useSaveApplicationVersion('p1', 1, 7), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.save({
        name: 'base',
        agentType: 'pipeline',
        instructions: 'entry_point: Agent 1\n',
        conversationStarters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        llmSettings: undefined,
        tags: [],
        tools: [],
        pipelineSettings: {
          nodes: [{ id: 'Agent 1' }],
          edges: [],
          orientation: 'vertical',
          layout_version: '1.0',
        },
      });
    });

    expect(capturedBody['pipeline_settings']).toEqual({
      nodes: [{ id: 'Agent 1' }],
      edges: [],
      orientation: 'vertical',
      layout_version: '1.0',
    });
    expect(capturedBody['instructions']).toBe('entry_point: Agent 1\n');
  });

  it('omits the pipeline_settings key entirely when the draft has none, so an agent save cannot blank a stored graph', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        capturedBody = (await info.request.json()) as Record<string, unknown>;
        return { id: '7', application_id: '1', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useSaveApplicationVersion('p1', 1, 7), {
      wrapper: createWrapper(),
    });

    await act(async () => {
      await result.current.save({
        name: 'base',
        agentType: undefined,
        instructions: 'Do the thing',
        conversationStarters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        llmSettings: undefined,
        tags: [],
        tools: [],
        pipelineSettings: undefined,
      });
    });

    expect(Object.keys(capturedBody)).not.toContain('pipeline_settings');
  });
});

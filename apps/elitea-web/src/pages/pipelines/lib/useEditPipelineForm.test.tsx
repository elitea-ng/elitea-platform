import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getUpdateApplicationVersionMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

// Deep imports into the slice's own stores: test files are excluded from
// dependency-cruiser's `no-deep-slice-import` fence (.dependency-cruiser.cjs
// `options.exclude`), and driving the real stores is the only honest way to
// assert what the save handler reads out of the live editor.
import { usePipelineEditorStore } from '@/features/pipelines/model/pipelineEditorStore';
import { usePipelineYamlStore } from '@/features/pipelines/model/pipelineYamlStore';

import { useEditPipelineForm } from './useEditPipelineForm';
import { useEditPipelineVersionFields } from './useEditPipelineVersionFields';

/**
 * The page's own composition: the version-level form state and the save hook
 * that reads it. Rendered together rather than passing a hand-built fields
 * object, because the pair IS the unit under test — the save body is built
 * from what `useEditPipelineVersionFields` seeded off the same version.
 */
function useEditPipelineFormUnderTest(
  detail: ApplicationDetail | undefined,
  activeVersion: ApplicationVersionDetail | undefined,
  projectId: string | undefined,
  applicationId: number | undefined,
) {
  const versionFields = useEditPipelineVersionFields(activeVersion);
  return { ...useEditPipelineForm(detail, activeVersion, projectId, applicationId, versionFields), versionFields };
}

const DETAIL: ApplicationDetail = {
  id: '42',
  name: 'My Pipeline',
  description: 'A helpful pipeline',
  icon: '',
  owner_id: 'user-1',
  created_at: '2026-01-01T00:00:00Z',
  versions: [],
};

const VERSION: ApplicationVersionDetail = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  agent_type: 'pipeline',
  instructions: 'Be helpful.',
  conversation_starters: ['Hi there'],
};

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
  // Both stores are session-wide singletons — reset so one test's live-editor
  // state cannot leak into the next.
  usePipelineYamlStore.setState({ yamlCode: '' });
  usePipelineEditorStore.setState({ nodes: [], edges: [] });
});

describe('useEditPipelineForm', () => {
  it('seeds the form from detail/activeVersion', () => {
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });
    expect(result.current.form.getValues()).toEqual({
      name: 'My Pipeline',
      description: 'A helpful pipeline',
      version_details: { conversation_starters: ['Hi there'] },
    });
  });

  it('seeds empty defaults while detail has not loaded yet', () => {
    const { result } = renderHook(() => useEditPipelineFormUnderTest(undefined, undefined, '9', 42), { wrapper });
    expect(result.current.form.getValues()).toEqual({
      name: '',
      description: '',
      version_details: { conversation_starters: [] },
    });
  });

  it('handleSave is a no-op (does not call the save endpoint) when activeVersion is undefined', () => {
    const saveSpy = vi.fn(() => ({ id: '1', application_id: '42', name: 'base', status: 'draft' }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, undefined, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('handleSave calls the save endpoint with agentType forced to "pipeline"', async () => {
    let sentAgentType: unknown;
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        const body = (await info.request.json()) as { agent_type?: unknown };
        sentAgentType = body.agent_type;
        return { id: '1', application_id: '42', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(sentAgentType).toBe('pipeline'));
  });

  // #135: handleSave used to submit `toVersionDraft(activeVersion,
  // conversationStarters)` and nothing else — the live canvas never reached
  // the wire, the PUT answered 200, and the graph was gone on reload.
  it('handleSave sends the LIVE editor YAML as instructions plus the laid-out pipeline_settings', async () => {
    /*
     * A REAL one-node graph, not `id: Agent 1`. A space fails the compiler's
     * `valid_graph_id` (`yaml.rs:362`), and `handleSave` now refuses a
     * document the runtime would refuse — so the old fixture was asserting
     * that an inadmissible graph reaches the wire, which is the opposite of
     * what this test is for. Same fixture `EditPipeline.test.tsx` uses.
     */
    const liveYaml = 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n';
    usePipelineYamlStore.setState({ yamlCode: liveYaml });
    usePipelineEditorStore.setState({ nodes: [], edges: [] });

    let body: Record<string, unknown> = {};
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        body = (await info.request.json()) as Record<string, unknown>;
        return { id: '1', application_id: '42', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(body['instructions']).toBe(liveYaml));
    const settings = body['pipeline_settings'] as { nodes: readonly { id: string }[] };
    expect(settings.nodes.map((node) => node.id)).toContain('Printer_1');
  });

  it('handleSave falls back to the stored instructions and sends no pipeline_settings when the editor holds nothing', async () => {
    usePipelineYamlStore.setState({ yamlCode: '' });

    let body: Record<string, unknown> = {};
    server.use(
      getUpdateApplicationVersionMockHandler(async (info) => {
        body = (await info.request.json()) as Record<string, unknown>;
        return { id: '1', application_id: '42', name: 'base', status: 'draft' };
      }),
    );
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(body['instructions']).toBe('Be helpful.'));
    expect(Object.keys(body)).not.toContain('pipeline_settings');
  });

  it('isSaving reflects the in-flight save state', () => {
    const saveSpy = vi.fn(() => ({ id: '1', application_id: '42', name: 'base', status: 'draft' }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    expect(result.current.isSaving).toBe(false);
  });

  it('saveError is undefined before any save attempt', () => {
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });
    expect(result.current.saveError).toBeUndefined();
  });

  it('saveError surfaces once a save attempt fails — old app: useSaveVersion.js toasts on every failed save; this app has no toast infra, so the caller renders this instead', async () => {
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(result.current.saveError).toBeDefined());
    expect(result.current.isSaving).toBe(false);
  });

  /**
   * The veto's SECOND enforcement point, and the reason one was not enough.
   *
   * `GraphAdmissionGate` publishes the refusal as an RHF `root.*` error,
   * which disables the Save button — and `form.handleSubmit` deletes every
   * `root.*` error before deciding whether to submit (react-hook-form 7.83:
   * the resolver's errors are assigned wholesale, then
   * `unset(_formState.errors, 'root')`, `dist/index.esm.mjs:2989/3002`). So
   * the submit path itself had no admission check: any route to a Save click
   * that bypassed the disabled button PUT the inadmissible document. One is
   * reachable today — `useRefetchPipelineAfterSave` fires a detail refetch
   * after every save, and a failed one puts `ConfigurationTab` into its error
   * branch, which unmounts the gate while `EditPipeline` keeps the Save bar.
   *
   * Note this test never renders the gate at all, which is the point: it
   * asserts that the SAVE PATH refuses on its own.
   */
  it('handleSave refuses to store a graph the runtime would refuse, and says so', async () => {
    // `Agent 1` — a space is not a legal graph id (`yaml.rs:362`).
    usePipelineYamlStore.setState({ yamlCode: 'entry_point: Agent 1\nnodes:\n  - id: Agent 1\n    type: llm\n' });
    const saveSpy = vi.fn(() => ({ id: '1', application_id: '42', name: 'base', status: 'draft' }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(result.current.admissionRefused).toBe(true));
    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('handleSave clears the admission refusal once the graph is fixed', async () => {
    usePipelineYamlStore.setState({ yamlCode: 'entry_point: Agent 1\nnodes:\n  - id: Agent 1\n    type: llm\n' });
    server.use(
      getUpdateApplicationVersionMockHandler({ id: '1', application_id: '42', name: 'base', status: 'draft' }),
    );
    const { result } = renderHook(() => useEditPipelineFormUnderTest(DETAIL, VERSION, '9', 42), { wrapper });

    act(() => {
      result.current.handleSave();
    });
    await waitFor(() => expect(result.current.admissionRefused).toBe(true));

    act(() => {
      usePipelineYamlStore.setState({
        yamlCode: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      });
      result.current.handleSave();
    });

    await waitFor(() => expect(result.current.admissionRefused).toBe(false));
  });

  it('saveError clears once a subsequent save attempt succeeds', async () => {
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result, rerender } = renderHook(
      ({ detail, version }: { detail: ApplicationDetail; version: ApplicationVersionDetail }) =>
        useEditPipelineFormUnderTest(detail, version, '9', 42),
      { wrapper, initialProps: { detail: DETAIL, version: VERSION } },
    );

    act(() => {
      result.current.handleSave();
    });
    await waitFor(() => expect(result.current.saveError).toBeDefined());

    server.use(
      getUpdateApplicationVersionMockHandler({
        id: '1',
        application_id: '42',
        name: 'base',
        status: 'draft',
      }),
    );
    rerender({ detail: DETAIL, version: VERSION });

    act(() => {
      result.current.handleSave();
    });
    await waitFor(() => expect(result.current.saveError).toBeUndefined());
  });
});

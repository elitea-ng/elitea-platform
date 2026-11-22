import { act, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders, renderWithRouterAndProject } from '../../__tests__/testUtils';
import type { PickerToolEntry } from './toolPicker.types';
import { useApplyPipelineAttachments } from './useApplyPipelineAttachments';
import { useCreateInNewTab } from './useCreateInNewTab';
import { isVersionLocked, useIsVersionLocked } from './useIsVersionLocked';
import { usePagedPickerAccumulator } from './usePagedPickerList';
import { usePickerMenu } from './usePickerMenu';
import { usePipelineToolAttachments } from './usePipelineToolAttachments';
import { useReconcilePipelineAttachments } from './useReconcilePipelineAttachments';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

function mountHookWithRouter<T>(useHook: () => T): { readonly current: () => T } {
  let latest: T | undefined;
  function Probe() {
    latest = useHook();
    return null;
  }
  renderWithRouterAndProject(<Probe />, PROJECT_ID);
  return {
    current: () => {
      if (latest === undefined) throw new Error('Hook has not mounted yet');
      return latest;
    },
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('tool-picker hooks', () => {
  it('usePickerMenu manages open/hasOpened/search/close state', () => {
    const { result } = renderHookWithProviders(() => usePickerMenu());
    expect(result.current.isOpen).toBe(false);
    expect(result.current.hasOpened).toBe(false);

    const anchor = document.createElement('button');
    act(() => {
      result.current.openMenu(anchor);
      result.current.setSearch('github');
    });
    expect(result.current.isOpen).toBe(true);
    expect(result.current.hasOpened).toBe(true);
    expect(result.current.search).toBe('github');

    act(() => {
      result.current.closeMenu();
    });
    expect(result.current.isOpen).toBe(false);
    expect(result.current.hasOpened).toBe(true);
    expect(result.current.search).toBe('');
  });

  it('useCreateInNewTab opens safe internal routes with noopener,noreferrer and rejects unsafe routes', () => {
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null);
    const onReturn = vi.fn();
    const { result } = renderHookWithProviders(() => useCreateInNewTab(onReturn));

    act(() => {
      result.current('https://evil.example/phish');
      result.current('//evil.example/phish');
      result.current('/toolkits/../admin');
      result.current('/toolkits\\admin');
      result.current('/toolkits/%2fadmin');
      result.current('/toolkits/create\r\nX-Header: 1');
    });
    expect(openSpy).not.toHaveBeenCalled();

    act(() => {
      result.current('/toolkits/create');
    });
    expect(openSpy).toHaveBeenCalledWith(
      `${window.location.origin}/toolkits/create`,
      '_blank',
      'noopener,noreferrer',
    );

    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    expect(onReturn).toHaveBeenCalledTimes(1);

    // Second focus does not re-trigger
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    expect(onReturn).toHaveBeenCalledTimes(1);
  });

  it('useIsVersionLocked locks published and embedded versions only', () => {
    expect(isVersionLocked('published')).toBe(true);
    expect(isVersionLocked('embedded')).toBe(true);
    expect(isVersionLocked('draft')).toBe(false);
    expect(isVersionLocked(undefined)).toBe(false);

    const { result } = renderHookWithProviders(() => useIsVersionLocked('published'));
    expect(result.current).toBe(true);
  });

  it('usePipelineToolAttachments fetches toolkit details, blocks Swarm agents, and updates versions', async () => {
    const onToolsChange = vi.fn();
    const onError = vi.fn();

    server.use(
      http.get(`${BASE}/elitea_core/tool/prompt_lib/${PROJECT_ID}/15`, () =>
        HttpResponse.json({
          id: 15,
          type: 'github',
          name: 'GitHub Prod',
          settings: { selected_tools: ['list_issues'] },
        }),
      ),
      http.get(`${BASE}/elitea_core/application/prompt_lib/${PROJECT_ID}/77`, () =>
        HttpResponse.json({
          id: 77,
          name: 'SwarmAgent',
          version_details: { id: 1, meta: { internal_tools: ['swarm'] } },
        }),
      ),
    );

    const initial: PickerToolEntry[] = [
      { type: 'application', name: 'Helper', settings: { application_id: 20, application_version_id: 1 } },
    ];
    const probe = mountHookWithRouter(() =>
      usePipelineToolAttachments({
        tools: initial,
        onToolsChange,
        projectId: PROJECT_ID,
        onError,
      }),
    );
    await waitFor(() => expect(probe.current()).toBeDefined());

    const attached = await act(async () =>
      probe.current().attachToolkit({ id: 15, name: 'GitHub Prod', type: 'github' }),
    );
    expect(attached?.toolkit_name).toBe('GitHubProd');
    expect(onToolsChange).toHaveBeenCalled();

    const blocked = await act(async () =>
      probe.current().prepareApplicationAttachment({ id: 77, name: 'SwarmAgent' }),
    );
    expect(blocked).toBeNull();
    expect(onError).toHaveBeenCalledWith(expect.stringContaining('Swarm Mode'));

    act(() => {
      probe.current().setApplicationVersion(20, { id: 2 });
    });
    const lastCall = onToolsChange.mock.calls.at(-1)?.[0] as PickerToolEntry[];
    expect(lastCall.find((t) => t.settings?.application_id === 20)?.settings?.application_version_id).toBe(2);
  });

  it('useApplyPipelineAttachments applies added/removed relations and tolerates idempotent errors', async () => {
    const calls: string[] = [];
    server.use(
      http.patch(`${BASE}/elitea_core/tool/prompt_lib/${PROJECT_ID}/10`, () => {
        calls.push('patch-toolkit-10');
        return HttpResponse.json({ ok: true });
      }),
      http.patch(`${BASE}/elitea_core/tool/prompt_lib/${PROJECT_ID}/11`, () => {
        calls.push('remove-toolkit-11');
        return HttpResponse.json({ error: 'No such toolkit' }, { status: 404 });
      }),
      http.patch(`${BASE}/elitea_core/application_relation/prompt_lib/${PROJECT_ID}/55/2`, () => {
        calls.push('add-app-55');
        return HttpResponse.json({ tool_id: 901 });
      }),
    );

    const onError = vi.fn();
    const probe = mountHookWithRouter(() =>
      useApplyPipelineAttachments({ projectId: PROJECT_ID, onError }),
    );
    await waitFor(() => expect(probe.current()).toBeDefined());

    const outcome = await act(async () =>
      probe.current().applyAttachments({
        applicationId: 100,
        versionId: 1,
        initialTools: [{ id: 11, type: 'jira', toolkit_name: 'jira' }],
        tools: [
          { id: 10, type: 'github', toolkit_name: 'gh', settings: { selected_tools: ['list_issues'] } },
          { type: 'application', name: 'SubBot', settings: { application_id: 55, application_version_id: 2 } },
        ],
      }),
    );

    expect(outcome).not.toBe(false);
    expect(calls).toEqual(['patch-toolkit-10', 'add-app-55', 'remove-toolkit-11']);
    expect(onError).not.toHaveBeenCalled();
    if (outcome !== false) {
      expect(outcome.tools.find((t) => t.name === 'SubBot')?.id).toBe(901);
    }
  });

  it('useReconcilePipelineAttachments reconciles tools against current YAML', async () => {
    const onToolsChange = vi.fn();
    const initialTools: PickerToolEntry[] = [
      { id: 1, type: 'github', toolkit_name: 'gh', settings: { selected_tools: ['list_issues'] } },
    ];

    mountHookWithRouter(() =>
      useReconcilePipelineAttachments({
        tools: initialTools,
        initialTools,
        initialInstructions: 'nodes:\n  - id: n1\n    type: toolkit\n    toolkit_name: gh\n    tool: list_issues\n',
        initYamlCode: 'nodes:\n  - id: n1\n    type: toolkit\n    toolkit_name: gh\n    tool: list_issues\n',
        yamlJsonObject: { nodes: [] },
        onToolsChange,
        projectId: PROJECT_ID,
      }),
    );

    await waitFor(() => expect(onToolsChange).toHaveBeenCalledWith([]));
  });

  it('useApplyPipelineAttachments rejects traversal/missing IDs before issuing HTTP requests (S1)', async () => {
    const httpCalls: string[] = [];
    server.use(
      http.patch(`${BASE}/*`, ({ request }) => {
        httpCalls.push(request.url);
        return HttpResponse.json({ ok: true });
      }),
    );

    const onError = vi.fn();
    const badProjectProbe = mountHookWithRouter(() =>
      useApplyPipelineAttachments({ projectId: '../etc', onError }),
    );
    await waitFor(() => expect(badProjectProbe.current()).toBeDefined());

    const badProjectOutcome = await act(async () =>
      badProjectProbe.current().applyAttachments({
        applicationId: 100,
        versionId: 1,
        initialTools: [],
        tools: [{ id: 10, type: 'github', toolkit_name: 'gh' }],
      }),
    );
    expect(badProjectOutcome).toBe(false);

    const goodProjectProbe = mountHookWithRouter(() =>
      useApplyPipelineAttachments({ projectId: PROJECT_ID, onError }),
    );
    await waitFor(() => expect(goodProjectProbe.current()).toBeDefined());

    const missingToolkitIdOutcome = await act(async () =>
      goodProjectProbe.current().applyAttachments({
        applicationId: 100,
        versionId: 1,
        initialTools: [],
        tools: [{ type: 'github', toolkit_name: 'gh' }],
      }),
    );
    expect(missingToolkitIdOutcome).toBe(false);

    const invalidAppIdOutcome = await act(async () =>
      goodProjectProbe.current().applyAttachments({
        applicationId: 100,
        versionId: 1,
        initialTools: [],
        tools: [{ type: 'application', name: 'BadApp', settings: { application_id: 0, application_version_id: 1 } }],
      }),
    );
    expect(invalidAppIdOutcome).toBe(false);
    expect(httpCalls).toEqual([]);
    expect(onError).toHaveBeenCalledTimes(3);
  });

  it('usePagedPickerAccumulator replaces refetched pages idempotently without duplicating rows (F3)', () => {
    const setPage = vi.fn();
    const refetch = vi.fn();
    const { result, rerender } = renderHookWithProviders(
      (props: { page: number; pageRows: readonly string[]; isFetching: boolean }) =>
        usePagedPickerAccumulator<string>({
          page: props.page,
          pageSize: 2,
          queryKey: 'proj-1:',
          enabled: true,
          pageRows: props.pageRows,
          total: 4,
          isLoading: false,
          isFetching: props.isFetching,
          isError: false,
          refetch,
          setPage,
        }),
      undefined,
      { initialProps: { page: 0, pageRows: ['a', 'b'], isFetching: false } },
    );
    expect(result.current.rows).toEqual(['a', 'b']);

    // Advance to page 1
    rerender({ page: 1, pageRows: ['c', 'd'], isFetching: false });
    expect(result.current.rows).toEqual(['a', 'b', 'c', 'd']);

    // Simulate background refetch of page 1 settling with updated/same rows — must NOT duplicate
    rerender({ page: 1, pageRows: ['c', 'd'], isFetching: true });
    rerender({ page: 1, pageRows: ['c', 'd-updated'], isFetching: false });
    expect(result.current.rows).toEqual(['a', 'b', 'c', 'd-updated']);
  });
});

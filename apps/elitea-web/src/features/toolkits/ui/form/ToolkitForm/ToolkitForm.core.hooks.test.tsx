import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../../__tests__/testUtils';

import { useToolkitFormCore } from './ToolkitForm.core.hooks';
import type { CoreState } from './ToolkitForm.core.hooks';
import type { ResolvedToolkitFormProps } from './ToolkitForm.types';

const TOOLKIT_TYPES_URL = '/api/v2/elitea_core/toolkits/prompt_lib/:projectId';

const DISCOVER_TOOLS_URL = '/api/v2/elitea_core/toolkit_discover_tools/prompt_lib/:projectId/:toolkitType';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({})));
  // The catalogue read (#440) runs for a type that declares no tools of its
  // own. A per-test `server.use` still wins: msw puts run-time handlers first.
  server.use(http.post(DISCOVER_TOOLS_URL, () => HttpResponse.json({ tools: [], total: 0 })));
});

afterEach(() => {
  resetGeneratedClient();
});

function baseProps(overrides: Partial<ResolvedToolkitFormProps> = {}): ResolvedToolkitFormProps {
  const editToolDetail = { type: 'github', settings: { embedding_model: 'old-model' } };
  return {
    editToolDetail,
    onChangeToolDetail: vi.fn(),
    isEditing: true,
    hasNotSavedCredentials: false,
    isViewToggleVisible: true,
    hideConfigurationNameInput: false,
    showOnlyRequiredFields: false,
    showOnlyConfigurationFields: false,
    showNameFieldForcedly: false,
    showToolkitIcon: false,
    hideNameDescriptionInput: false,
    hideNameInput: false,
    hideOperationButtons: false,
    forceCustomView: false,
    isTeamProject: false,
    projectId: 'proj-1',
    formValues: editToolDetail,
    formInitialValues: editToolDetail,
    onSave: vi.fn().mockResolvedValue({}),
    ...overrides,
  };
}

/**
 * Mounts `useToolkitFormCore` under a real router root context AND a real
 * socket client — the hook's own `useGetCurrentToolkitSchemas` call bottoms
 * out at both (`useSelectedProjectId`/`useSocketClient`), same as
 * `useGetCurrentToolkitSchemas.hooks.test.tsx`'s own `renderToolkitSchemas`
 * harness. Not reused from `../../../__tests__/testUtils.tsx` because that
 * file's `renderHookWithRouterAndProject` has no Socket provider, and this
 * file is outside this fix's editable scope (STRICT file-scope fence).
 */
function renderCore(props: ResolvedToolkitFormProps): { readonly box: { current: CoreState | undefined } } {
  const box: { current: CoreState | undefined } = { current: undefined };

  function ProbeComponent() {
    box.current = useToolkitFormCore(props);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => props.projectId } },
  });

  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  return { box };
}

describe('useToolkitFormCore editField', () => {
  /**
   * [R1 regression] Baseline: `ToolkitForm.jsx:286-291` — when a child
   * selector auto-selects a fallback value on the user's behalf (`options:
   * { isAutoSelect: true }`), `editField` also calls Formik's own
   * `resetForm({ values: updatedValues })` so the form's OWN initial values
   * move to match, and the auto-correction never reads as a user edit. This
   * app has no ambient Formik context, so the explicit `onResetForm` prop is
   * the equivalent hook a caller wires up. Before this fix, `editField`
   * dropped the whole branch — `onResetForm` was never even destructured
   * from props, let alone called — so this assertion fails against the
   * pre-fix code (confirmed by reverting the fix locally and re-running:
   * `onResetForm` stays uncalled) and passes once the branch is restored.
   */
  it('calls onResetForm with the auto-selected value merged into formValues when options.isAutoSelect is true', async () => {
    const onChangeToolDetail = vi.fn();
    const onResetForm = vi.fn();
    const props = baseProps({ onChangeToolDetail, onResetForm });
    const { box } = renderCore(props);

    await waitFor(() => expect(box.current).toBeDefined());

    await act(async () => {
      await box.current?.editField('settings.embedding_model', 'new-model', undefined, { isAutoSelect: true });
    });

    expect(onResetForm).toHaveBeenCalledTimes(1);
    expect(onResetForm).toHaveBeenCalledWith({ type: 'github', settings: { embedding_model: 'new-model' } });
    // The real field change still goes through `onChangeToolDetail`, options forwarded unchanged.
    expect(onChangeToolDetail).toHaveBeenCalledWith(expect.any(Function), { isAutoSelect: true });
  });

  it('does not call onResetForm for a normal user edit (no isAutoSelect option)', async () => {
    const onResetForm = vi.fn();
    const props = baseProps({ onResetForm });
    const { box } = renderCore(props);

    await waitFor(() => expect(box.current).toBeDefined());

    await act(async () => {
      await box.current?.editField('settings.embedding_model', 'new-model');
    });

    expect(onResetForm).not.toHaveBeenCalled();
  });

  it('does not throw when onResetForm is not supplied, even with isAutoSelect: true', async () => {
    const props = baseProps({ onResetForm: undefined });
    const { box } = renderCore(props);

    await waitFor(() => expect(box.current).toBeDefined());

    await expect(
      act(async () => {
        await box.current?.editField('settings.embedding_model', 'new-model', undefined, { isAutoSelect: true });
      }),
    ).resolves.not.toThrow();
  });

  /**
   * #440. `effectiveToolSchema` was the static schema and nothing else, so a
   * toolkit type that publishes its tools at run time offered none in the
   * "Tools" section — the same empty section a lost read produced. The cases
   * below discriminate a list, a failure, and a real empty catalogue.
   */
  describe('dynamic tool catalogue (#440)', () => {
    /** A type whose settings schema declares a `selected_tools` array with no tools of its own. */
    const RUNTIME_TYPE_SCHEMAS = { openapi_tool: { properties: { selected_tools: { type: 'array' } } } };
    const runtimeDetail = { type: 'openapi_tool', settings: {} };

    function readEnum(box: { current: CoreState | undefined }): unknown {
      const properties = box.current?.effectiveToolSchema?.properties as Record<string, { items?: { enum?: unknown } }> | undefined;
      return properties?.['selected_tools']?.items?.enum;
    }

    it('writes the published tool names into selected_tools.items.enum, where the Tools section reads them', async () => {
      server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json(RUNTIME_TYPE_SCHEMAS)));
      server.use(http.post(DISCOVER_TOOLS_URL, () => HttpResponse.json({ tools: [{ id: '1', name: 'alpha_op', type: 'openapi_tool' }], total: 1 })));

      const { box } = renderCore(baseProps({ editToolDetail: runtimeDetail, formValues: runtimeDetail, formInitialValues: runtimeDetail }));

      await waitFor(() => expect(readEnum(box)).toEqual(['alpha_op']));
      expect(box.current?.toolListReadFailed).toBe(false);
    });

    it('reports a failed catalogue read as its own state, with the enum still empty', async () => {
      server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json(RUNTIME_TYPE_SCHEMAS)));
      server.use(http.post(DISCOVER_TOOLS_URL, () => HttpResponse.json({ error: 'discover tools failed' }, { status: 500 })));

      const { box } = renderCore(baseProps({ editToolDetail: runtimeDetail, formValues: runtimeDetail, formInitialValues: runtimeDetail }));

      await waitFor(() => expect(box.current?.toolListReadFailed).toBe(true));
      expect(readEnum(box)).toBeUndefined();
    });

    it('reports a successful read with no tools as no failure, with the enum still empty', async () => {
      server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json(RUNTIME_TYPE_SCHEMAS)));
      let requestCount = 0;
      server.use(
        http.post(DISCOVER_TOOLS_URL, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [], total: 0 });
        }),
      );

      const { box } = renderCore(baseProps({ editToolDetail: runtimeDetail, formValues: runtimeDetail, formInitialValues: runtimeDetail }));

      await waitFor(() => expect(requestCount).toBe(1));
      await waitFor(() => expect(box.current?.toolListReadFailed).toBe(false));
      expect(readEnum(box)).toBeUndefined();
    });

    it('keeps the declared tool list and reads no catalogue for a type that declares its own tools', async () => {
      let requestCount = 0;
      server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({ github: { properties: { selected_tools: { items: { enum: ['create_issue'] } } } } })));
      server.use(
        http.post(DISCOVER_TOOLS_URL, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [{ id: '1', name: 'should_not_appear', type: 'github' }], total: 1 });
        }),
      );

      const { box } = renderCore(baseProps());

      await waitFor(() => expect(readEnum(box)).toEqual(['create_issue']));
      expect(requestCount).toBe(0);
      expect(box.current?.toolListReadFailed).toBe(false);
    });

    it('reports a failed toolkit type schema read as a failed tool list', async () => {
      server.use(http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({ error: 'schemas unavailable' }, { status: 500 })));

      const { box } = renderCore(baseProps({ editToolDetail: runtimeDetail, formValues: runtimeDetail, formInitialValues: runtimeDetail }));

      await waitFor(() => expect(box.current?.toolListReadFailed).toBe(true));
    });
  });
});

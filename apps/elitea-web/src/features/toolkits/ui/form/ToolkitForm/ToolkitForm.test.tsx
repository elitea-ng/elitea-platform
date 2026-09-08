import { act, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ToolEvents } from '@/entities/toolkit';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterSocketAndProject } from '../../../__tests__/testUtils';
import { eventEmitter } from '@/shared/lib/eventEmitter';

import type { ToolkitFormEditDetail, ToolkitFormProps, ToolkitValidationInjected } from './ToolkitForm';
import { ToolkitForm } from './ToolkitForm';

const TOOLKIT_TYPES_URL = '/api/v2/elitea_core/toolkits/prompt_lib/:projectId';
const CONFIGURATIONS_LIST_URL = '/api/v2/configurations/configurations/:projectId';
const CONFIGURATIONS_AVAILABLE_URL = '/api/v2/configurations/available/';
const DISCOVER_TOOLS_URL = '/api/v2/elitea_core/toolkit_discover_tools/prompt_lib/:projectId/:toolkitType';

/**
 * `ToolkitForm.tsx` fires all four of these on mount
 * (`useGetCurrentToolkitSchemas`, `useConfigurationsList`,
 * `useConfigurationsAsSchema` — see that file's own module doc comment,
 * redesign 4 — plus the #440 tool catalogue for a type that declares no
 * tools of its own). `src/test/setup.ts` runs MSW with
 * `onUnhandledRequest: 'error'`, so every one needs a handler or the test
 * fails outright, not just produces a wrong result.
 */
function mockToolkitFormEndpoints(toolkitTypeSchemas: Record<string, unknown> = {}): void {
  server.use(
    http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json(toolkitTypeSchemas)),
    http.get(CONFIGURATIONS_LIST_URL, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })),
    http.get(CONFIGURATIONS_AVAILABLE_URL, () => HttpResponse.json([])),
    http.post(DISCOVER_TOOLS_URL, () => HttpResponse.json({ tools: [], total: 0 })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function baseProps(overrides: Partial<ToolkitFormProps> = {}): ToolkitFormProps {
  const editToolDetail: ToolkitFormEditDetail = { type: 'custom', name: 'My Tool', settings: {} };
  return {
    editToolDetail,
    onChangeToolDetail: vi.fn(),
    isEditing: true,
    projectId: 'proj-1',
    formValues: editToolDetail,
    formInitialValues: editToolDetail,
    onSave: vi.fn().mockResolvedValue({}),
    ...overrides,
  };
}

describe('ToolkitForm', () => {
  it('hides the Form/Raw Json toggle for the custom toolkit type and renders ToolCustom', async () => {
    mockToolkitFormEndpoints();
    const { getByText, queryByText } = renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps()} />,
      'proj-1',
    );

    await waitFor(() => expect(getByText('JSON')).toBeInTheDocument());
    expect(queryByText('Form')).not.toBeInTheDocument();
    expect(queryByText('Raw Json')).not.toBeInTheDocument();
  });

  it('shows the Form/Raw Json toggle for a non-custom type once a schema object is resolved', async () => {
    mockToolkitFormEndpoints();
    const editToolDetail: ToolkitFormEditDetail = { type: 'some-other-type', name: 'x', settings: {} };
    const { getByText } = renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ editToolDetail, formValues: editToolDetail, formInitialValues: editToolDetail })} />,
      'proj-1',
    );

    await waitFor(() => expect(getByText('JSON')).toBeInTheDocument());
    expect(getByText('Form')).toBeInTheDocument();
    expect(getByText('Raw Json')).toBeInTheDocument();
  });

  it('hideOperationButtons unmounts ToolkitsOperationButtons: a ToolkitsUpdateToolkit event no longer reaches onSave', async () => {
    mockToolkitFormEndpoints();
    const onSave = vi.fn().mockResolvedValue({});
    renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ onSave, hideOperationButtons: true })} />,
      'proj-1',
    );
    await waitFor(() => expect(onSave).not.toHaveBeenCalled());

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    // No listener is registered while hideOperationButtons is set — give any
    // (incorrect) async dispatch a turn to land before asserting the negative.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(onSave).not.toHaveBeenCalled();
  });

  it('the same ToolkitsUpdateToolkit event DOES reach onSave once ToolkitsOperationButtons is mounted (hideOperationButtons unset)', async () => {
    mockToolkitFormEndpoints();
    const onSave = vi.fn().mockResolvedValue({});
    renderWithRouterSocketAndProject(<ToolkitForm {...baseProps({ onSave })} />, 'proj-1');
    await waitFor(() => expect(onSave).not.toHaveBeenCalled());

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
  });

  it('reports hasErrors: false once loaded when the toolkit has a name and no injected validation error', async () => {
    mockToolkitFormEndpoints();
    const onValidationStateChange = vi.fn();
    renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ onValidationStateChange })} />,
      'proj-1',
    );

    await waitFor(() => expect(onValidationStateChange).toHaveBeenCalledWith(expect.objectContaining({ hasErrors: false })));
  });

  it('reports hasErrors: true once an injected toolkitValidation error resolves to a server field error', async () => {
    mockToolkitFormEndpoints();
    const onValidationStateChange = vi.fn();
    const toolkitValidation: ToolkitValidationInjected = {
      isError: true,
      error: { data: { settings_errors: [{ msg: 'bad value', loc: ['settings', 'field1'] }] } },
      refetch: vi.fn(),
    };
    renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ onValidationStateChange, toolkitValidation })} />,
      'proj-1',
    );

    await waitFor(() => expect(onValidationStateChange).toHaveBeenCalledWith(expect.objectContaining({ hasErrors: true })));
  });

  it('syncs formValues.type from routeToolkitType via onSetFormField when no type is set yet', async () => {
    mockToolkitFormEndpoints();
    const onSetFormField = vi.fn();
    const editToolDetail: ToolkitFormEditDetail = { settings: {} };
    renderWithRouterSocketAndProject(
      <ToolkitForm
        {...baseProps({
          editToolDetail,
          formValues: {},
          formInitialValues: {},
          onSetFormField,
          routeToolkitType: 'jira',
        })}
      />,
      'proj-1',
    );

    await waitFor(() => expect(onSetFormField).toHaveBeenCalledWith('type', 'jira'));
  });

  it('does NOT sync the type when formValues already carries one', async () => {
    mockToolkitFormEndpoints();
    const onSetFormField = vi.fn();
    const editToolDetail: ToolkitFormEditDetail = { type: 'custom', settings: {} };
    renderWithRouterSocketAndProject(
      <ToolkitForm
        {...baseProps({
          editToolDetail,
          formValues: { type: 'confluence' },
          formInitialValues: { type: 'confluence' },
          onSetFormField,
          routeToolkitType: 'jira',
        })}
      />,
      'proj-1',
    );

    await waitFor(() => expect(onSetFormField).not.toHaveBeenCalledWith('type', expect.anything()));
  });

  it('renders a loading spinner (and nothing else) while editToolDetail.isLoadingConfigurations is set', async () => {
    mockToolkitFormEndpoints();
    const editToolDetail: ToolkitFormEditDetail = { type: 'custom', name: 'x', settings: {}, isLoadingConfigurations: true };
    const { getByRole, queryByText } = renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ editToolDetail, formValues: editToolDetail, formInitialValues: editToolDetail })} />,
      'proj-1',
    );

    await waitFor(() => expect(getByRole('progressbar')).toBeInTheDocument());
    expect(queryByText('JSON')).not.toBeInTheDocument();
  });

  /*
   * Issue 308 — the credential/model pickers rendered as blank space.
   * `ToolBaseProperty.dispatch.tsx`'s `renderCredentialLike` opens with
   * `if (!ctx.slots?.renderCredentialLikeField) return null`, and no file in
   * the repository supplied that slot, so the field silently disappeared.
   *
   * These drive the REAL composition root and deliberately pass NO `slots`
   * prop: if `ToolkitForm` stops supplying the default, they fail. Injecting
   * the slot from the test — which the existing `ToolBaseProperty.test.tsx`
   * cases do, correctly, for their own purpose — is exactly what let a
   * fully-tested renderer ship with no production caller.
   */
  const ARTIFACT_SCHEMA = {
    artifact: {
      type: 'object',
      properties: {
        // Served verbatim by the Go type catalogue
        // (internal/api/v2/toolkits/handler.go:162) and retyped to
        // `embedding_model` by `toolkitSchema.helpers.ts` on the property
        // NAME, which is why this is the one credential-like field a user
        // can actually reach today.
        embedding_model: { type: 'string' },
      },
    },
  };

  function mockEmbeddingModels(models: readonly Record<string, unknown>[]): void {
    server.use(
      http.get('/api/v2/configurations/models/:projectId', ({ request }) => {
        const section = new URL(request.url).searchParams.get('section');
        if (section !== 'embedding') return HttpResponse.json({ items: [], total: 0 });
        return HttpResponse.json({ items: models, total: models.length });
      }),
    );
  }

  it('renders the embedding-model picker with real options from the composition root, supplying no slot', async () => {
    mockToolkitFormEndpoints(ARTIFACT_SCHEMA);
    mockEmbeddingModels([
      { name: 'text-embedding-3-small', display_name: 'Embedding Small', project_id: 1 },
      { name: 'text-embedding-3-large', project_id: 1 },
    ]);
    const editToolDetail: ToolkitFormEditDetail = { type: 'artifact', name: 'a', settings: {} };
    const user = userEvent.setup();
    const { getByTestId, findByRole } = renderWithRouterSocketAndProject(
      <ToolkitForm {...baseProps({ editToolDetail, formValues: editToolDetail, formInitialValues: editToolDetail })} />,
      'proj-1',
    );

    // `toBeVisible`, NOT `toBeInTheDocument`: an empty `<Box>` satisfies the
    // latter (no layout box, still in the document), which is how a field
    // that rendered `null` inside its wrapper went unnoticed here before.
    await waitFor(() => getByTestId('model-select-embedding'));
    expect(await findByRole('combobox')).toBeVisible();

    // And it must offer the REAL rows the endpoint answered — a picker wired
    // to nothing would open an empty menu and still be "visible".
    await user.click(await findByRole('combobox'));
    expect(await findByRole('option', { name: 'Embedding Small' })).toBeVisible();
    expect(await findByRole('option', { name: 'text-embedding-3-large' })).toBeVisible();
  });

  it('captures a picked embedding model into the toolkit settings as the model NAME', async () => {
    mockToolkitFormEndpoints(ARTIFACT_SCHEMA);
    mockEmbeddingModels([{ name: 'text-embedding-3-small', display_name: 'Embedding Small', project_id: 1 }]);
    const onChangeToolDetail = vi.fn();
    const editToolDetail: ToolkitFormEditDetail = { type: 'artifact', name: 'a', settings: {} };
    const user = userEvent.setup();
    const { getByTestId, findByRole } = renderWithRouterSocketAndProject(
      <ToolkitForm
        {...baseProps({ editToolDetail, formValues: editToolDetail, formInitialValues: editToolDetail, onChangeToolDetail })}
      />,
      'proj-1',
    );

    await waitFor(() => getByTestId('model-select-embedding'));
    await user.click(await findByRole('combobox'));
    await user.click(await findByRole('option', { name: 'Embedding Small' }));

    /*
     * The NAME, never the synthesized `${project_id}_${name}` row id: the Go
     * handler resolves the saved value with `data->>'name' = $1`
     * (toolkits/handler.go:857-863), so persisting the id would make every
     * saved toolkit fail that existence check.
     */
    /*
     * `editField` calls `onChangeToolDetail` with an UPDATER, not a value
     * (`ToolkitForm.core.hooks.ts:122`), so the payload only exists once the
     * updater is applied. Asserting `toHaveBeenCalled()` alone would pass for
     * any field change at all — including none from this picker.
     */
    await waitFor(() => expect(onChangeToolDetail).toHaveBeenCalled());
    const [updater] = onChangeToolDetail.mock.calls.at(-1) as [
      (previous: Record<string, unknown>) => Record<string, unknown>,
    ];
    const next = updater({ type: 'artifact', name: 'a', settings: {} });

    /*
     * The NAME, never the synthesized `${project_id}_${name}` row id: the Go
     * handler resolves the saved value with `data->>'name' = $1`
     * (toolkits/handler.go:857-863), so persisting the id would make every
     * saved toolkit fail that existence check.
     */
    expect(next['settings']).toEqual(expect.objectContaining({ embedding_model: 'text-embedding-3-small' }));
  });

  /**
   * #440. The "Tools" section read nothing from the backend, so a toolkit
   * type that publishes its tools at run time offered none, and a lost read
   * looked exactly the same. The cases below discriminate the outcomes.
   */
  describe('dynamic tool catalogue (#440)', () => {
    /** A type whose settings schema declares a `selected_tools` array with no tools of its own, so the catalogue tier is the one in use. */
    const RUNTIME_TYPE_SCHEMAS = { openapi_tool: { properties: { selected_tools: { type: 'array' } } } };
    const editToolDetail: ToolkitFormEditDetail = { type: 'openapi_tool', name: 'My API', settings: {} };

    function renderRuntimeToolkit() {
      return renderWithRouterSocketAndProject(
        <ToolkitForm {...baseProps({ editToolDetail, formValues: editToolDetail, formInitialValues: editToolDetail })} />,
        'proj-1',
      );
    }

    /** The enum the "Tools" section reads is asserted in `ToolkitForm.core.hooks.test.tsx`; this file owns what the screen shows. */
    it('shows no error when the backend publishes tools for the toolkit type', async () => {
      mockToolkitFormEndpoints(RUNTIME_TYPE_SCHEMAS);
      let requestCount = 0;
      server.use(
        http.post(DISCOVER_TOOLS_URL, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [{ id: '1', name: 'alpha_op', type: 'openapi_tool' }], total: 1 });
        }),
      );

      const { queryByTestId } = renderRuntimeToolkit();

      await waitFor(() => expect(requestCount).toBe(1));
      await waitFor(() => expect(queryByTestId('toolkit-form-tool-list-error')).not.toBeInTheDocument());
    });

    it('shows an error with a retry, not an empty Tools section, when the catalogue read fails', async () => {
      mockToolkitFormEndpoints(RUNTIME_TYPE_SCHEMAS);
      server.use(http.post(DISCOVER_TOOLS_URL, () => HttpResponse.json({ error: 'discover tools failed' }, { status: 500 })));

      const { findByTestId, getByRole } = renderRuntimeToolkit();

      expect(await findByTestId('toolkit-form-tool-list-error')).toBeInTheDocument();
      expect(getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    });

    it('shows no error when the read succeeds and the toolkit type offers no tools', async () => {
      mockToolkitFormEndpoints(RUNTIME_TYPE_SCHEMAS);
      let requestCount = 0;
      server.use(
        http.post(DISCOVER_TOOLS_URL, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [], total: 0 });
        }),
      );

      const { queryByTestId } = renderRuntimeToolkit();

      await waitFor(() => expect(requestCount).toBe(1));
      await waitFor(() => expect(queryByTestId('toolkit-form-tool-list-error')).not.toBeInTheDocument());
    });

    it('does not read the catalogue for a type that declares its own tools', async () => {
      let requestCount = 0;
      mockToolkitFormEndpoints({ github: { properties: { selected_tools: { items: { enum: ['create_issue'] } } } } });
      server.use(
        http.post(DISCOVER_TOOLS_URL, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [], total: 0 });
        }),
      );

      const staticDetail: ToolkitFormEditDetail = { type: 'github', name: 'My GitHub', settings: {} };
      const { findByText, queryByTestId } = renderWithRouterSocketAndProject(
        <ToolkitForm {...baseProps({ editToolDetail: staticDetail, formValues: staticDetail, formInitialValues: staticDetail })} />,
        'proj-1',
      );

      await findByText('JSON');
      expect(requestCount).toBe(0);
      expect(queryByTestId('toolkit-form-tool-list-error')).not.toBeInTheDocument();
    });
  });
});

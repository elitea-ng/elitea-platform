import { createRef } from 'react';

import { act, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { forceResizeObserverAbsentForTest } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../__tests__/testUtils';
import type { PipelineCreateFormSlotProps, PipelineEditorDeps, PipelineEditorHandle, PipelineEditorShellProps } from './PipelineEditor';
import { PipelineEditor } from './PipelineEditor';

/**
 * The "threads … without crashing when switching to the Flow editor tab"
 * test below renders `PipelineEditorBody` -> `EditorPanel`, whose flow pane
 * only fails to load (the assertion this test relies on) when
 * `window.ResizeObserver` is unavailable — see
 * `EditorPanel.test.tsx`'s own module doc comment and
 * `codeMirrorTestPolyfills.ts`'s `forceResizeObserverAbsentForTest`. Without
 * this, the test is silently order-dependent on which other files ran in
 * the same vitest worker; reproduced directly (2 fail / 0 fail / 0 fail
 * across 3 consecutive full-suite runs with no source change).
 */
forceResizeObserverAbsentForTest();

/**
 * `./EditorPanel` (unit A2n) landed in this worktree partway through this
 * sub-unit's own build — see `PipelineEditor.tsx`'s own module doc comment
 * for the full landing-order account and the one disclosed `stopRun`
 * signature deviation it required. All tests in this file pass against the
 * real, landed `EditorPanel`.
 */

function buildDeps(overrides: Partial<PipelineEditorDeps> = {}): PipelineEditorDeps {
  return {
    renderShell: (props: PipelineEditorShellProps) => (
      <div data-testid="shell">
        <div data-testid="shell-title">{props.title}</div>
        <div data-testid="shell-form-content">{props.formContent}</div>
        <div data-testid="shell-save-button">{props.saveButton}</div>
        <div data-testid="shell-children">{props.children}</div>
      </div>
    ),
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('PipelineEditor', () => {
  it('renders nothing when there is no pipeline and it is not create mode', () => {
    const deps = buildDeps();
    const { container } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={null}
        isVisible
        isCreateMode={false}
        deps={deps}
      />,
      'p1',
    );

    expect(container.querySelector('[data-testid="shell"]')).toBeNull();
  });

  it('create mode: shows the create title and calls renderCreateForm with the local draft state', async () => {
    const renderCreateForm = vi.fn((_props: PipelineCreateFormSlotProps) => <div data-testid="create-form" />);
    const deps = buildDeps({ renderCreateForm });

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={null}
        isVisible
        isCreateMode
        deps={deps}
      />,
      'p1',
    );

    await findByTestId('shell-title');
    expect(renderCreateForm).toHaveBeenCalled();
    const call = renderCreateForm.mock.calls[0]?.[0];
    expect(call?.values.name).toBe('');
    expect(typeof call?.onFieldChange).toBe('function');
  });

  it('create mode: the save button is disabled until name and description are filled (schema-gated)', async () => {
    const deps = buildDeps({ renderCreateForm: () => <div data-testid="create-form" /> });

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={null}
        isVisible
        isCreateMode
        deps={deps}
      />,
      'p1',
    );

    const saveButtonSlot = await findByTestId('shell-save-button');
    const button = saveButtonSlot.querySelector('[data-testid="pipeline-save-button"]');
    expect(button).not.toBeNull();
    expect(button?.hasAttribute('disabled')).toBe(true);
  });

  it('edit mode: the save button is disabled when deps.onSaveVersion is not supplied', async () => {
    const deps = buildDeps();

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={{ id: 'p1', entity_meta: { id: 'p1' }, entity_settings: { version_id: 'v1' } }}
        isVisible={false}
        isCreateMode={false}
        deps={deps}
      />,
      'proj1',
    );

    const saveButtonSlot = await findByTestId('shell-save-button');
    const button = saveButtonSlot.querySelector('[data-testid="pipeline-save-button"]');
    expect(button?.hasAttribute('disabled')).toBe(true);
  });

  /*
   * The chat-embedded editor's Configuration tab (welcome message, tags,
   * variables, …) is rendered entirely through `deps.renderConfigurationPanels`
   * — a caller-owned form (`processes/chat`'s `useChatPipelineConfig.tsx`)
   * this component never sees the fields of. Neither `isDirty` (Flow tab)
   * nor `isYamlDirty` (YAML tab) is ever set by anything under that tab, so
   * before `deps.isConfigurationDirty` existed, editing ONLY a Configuration
   * field left Save permanently disabled even with a real `onSaveVersion`
   * wired — the chat-embedded-editor regression this dep closes.
   */
  it('edit mode: the save button stays disabled when onSaveVersion is supplied but nothing has actually changed', async () => {
    const deps = buildDeps({ onSaveVersion: vi.fn() });

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={{ id: 'p1', entity_meta: { id: 'p1' }, entity_settings: { version_id: 'v1' } }}
        isVisible={false}
        isCreateMode={false}
        deps={deps}
      />,
      'proj1',
    );

    const saveButtonSlot = await findByTestId('shell-save-button');
    const button = saveButtonSlot.querySelector('[data-testid="pipeline-save-button"]');
    expect(button?.hasAttribute('disabled')).toBe(true);
  });

  it('edit mode: deps.isConfigurationDirty arms the save button even though the Flow/YAML tabs were never touched', async () => {
    const deps = buildDeps({ onSaveVersion: vi.fn(), isConfigurationDirty: true });

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={{ id: 'p1', entity_meta: { id: 'p1' }, entity_settings: { version_id: 'v1' } }}
        isVisible={false}
        isCreateMode={false}
        deps={deps}
      />,
      'proj1',
    );

    const saveButtonSlot = await findByTestId('shell-save-button');
    const button = saveButtonSlot.querySelector('[data-testid="pipeline-save-button"]');
    expect(button?.hasAttribute('disabled')).toBe(false);
  });

  it('imperative handle: onRcvAgentEvent is a no-op when activeParticipantId does not match the pipeline', () => {
    const deps = buildDeps();
    const ref = createRef<PipelineEditorHandle>();

    renderWithRouterAndProject(
      <PipelineEditor
        ref={ref}
        pipeline={{ id: 'p1', entity_meta: { id: 'p1' } }}
        isVisible={false}
        isCreateMode={false}
        activeParticipantId="different-id"
        deps={deps}
      />,
      'proj1',
    );

    expect(() => ref.current?.onRcvAgentEvent({ type: 'noop' })).not.toThrow();
  });

  it('onClose calls onClosePipelineEditor and deps.onEditorClosed', async () => {
    const onClosePipelineEditor = vi.fn();
    const onEditorClosed = vi.fn();
    let capturedOnClose: (() => void) | undefined;
    const deps = buildDeps({
      onEditorClosed,
      renderShell: (props) => {
        capturedOnClose = props.onClose;
        return <div data-testid="shell" />;
      },
    });

    renderWithRouterAndProject(
      <PipelineEditor
        pipeline={null}
        isVisible
        isCreateMode
        onClosePipelineEditor={onClosePipelineEditor}
        deps={deps}
      />,
      'proj1',
    );

    await waitFor(() => expect(capturedOnClose).toBeDefined());
    act(() => capturedOnClose?.());

    expect(onClosePipelineEditor).toHaveBeenCalled();
    expect(onEditorClosed).toHaveBeenCalled();
  });

  it('edit mode: fetches version detail and forwards a fetch error to the shell', async () => {
    server.use(
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    let capturedError: unknown;
    const deps = buildDeps({
      renderShell: (props) => {
        capturedError = props.error;
        return <div data-testid="shell" />;
      },
    });

    renderWithRouterAndProject(
      <PipelineEditor
        pipeline={{ id: 42, entity_meta: { id: 42 }, entity_settings: { version_id: 7 } }}
        isVisible
        isCreateMode={false}
        deps={deps}
      />,
      'proj1',
    );

    await waitFor(() => expect(capturedError).toBeDefined());
  });

  it('edit mode: threads the fetched versionDetail\'s tools/llm_settings down to PipelineEditorBody\'s EditorPanel (versionTools/llmSettings) without crashing when switching to the Flow editor tab -- see flowEditorVersionInputs.helpers.test.ts for the wire->prop mapping\'s own field-level coverage; EditorPanel\'s lazy FlowWrapper/FlowEditor chain has a real, unrelated broken transitive dependency (see EditorPanel.test.tsx\'s own doc comment), so a value-level assertion that these two reach FlowEditor itself is not possible from this composition-root-level test', async () => {
    server.use(
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({
          id: '7',
          application_id: '42',
          name: 'v1',
          status: 'draft',
          tools: [{ id: 1, type: 'toolkit', name: 'search' }],
          llm_settings: { model_name: 'gpt-4o', temperature: 0.8, max_tokens: 2048 },
        }),
      ),
    );
    const deps = buildDeps();
    const user = userEvent.setup();

    const { findByTestId } = renderWithRouterAndProject(
      <PipelineEditor
        pipeline={{ id: 42, entity_meta: { id: 42 }, entity_settings: { version_id: 7 } }}
        isVisible
        isCreateMode={false}
        deps={deps}
      />,
      'proj1',
    );

    const formContent = await findByTestId('shell-form-content');
    await user.click(within(formContent).getByRole('tab', { name: 'Flow editor' }));

    const bodyContent = await findByTestId('shell-children');
    // The wait window is generous on purpose: this assertion depends on
    // `React.lazy(() => import('./FlowWrapper'))` REJECTING, and the first
    // file in a vitest worker to trigger that import pays the whole
    // transform cost for the chain. At 5s it was an order-dependent flake
    // (see this file's own note above); the assertion itself is unchanged.
    expect(await within(bodyContent).findByText('Failed to load the flow editor', {}, { timeout: 20000 })).toBeInTheDocument();
  }, 30000);
});

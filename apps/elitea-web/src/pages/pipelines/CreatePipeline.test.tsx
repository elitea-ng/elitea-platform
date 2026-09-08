import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { load } from 'js-yaml';
import { http, HttpResponse } from 'msw';

import { PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';
import { getCreateApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { useNavBlockerStore } from '@/widgets/app-shell';

import { CreatePipeline } from './CreatePipeline';
import { renderPipelinesRoute } from './__tests__/testRouter';

/**
 * The Instructions accordion on this page mounts a CodeMirror editor, and CM6
 * measures text layout on every animation frame. jsdom implements neither
 * `Range.prototype.getClientRects` nor `ResizeObserver`, so without these the
 * measurement throws asynchronously out of a `requestAnimationFrame` callback
 * and vitest reports the whole FILE as errored, whatever the assertions did.
 */
installCodeMirrorTestPolyfills();

/**
 * The model catalogue the Advanced-settings picker reads. Served for every
 * test here because the page mounts the picker unconditionally and
 * `src/test/setup.ts` runs msw with `onUnhandledRequest: 'error'`.
 */
const CATALOGUE = {
  items: [
    { name: 'gpt-4o', display_name: 'GPT-4o', project_id: '1', default: true },
    { name: 'qwen3.5', display_name: 'Qwen 3.5', project_id: '1' },
  ],
  default_model_name: 'gpt-4o',
};

/** Fills Name/Description so Save enables, then clicks it. */
async function fillAndSave(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.type(screen.getByTestId('agent-name-input'), 'my-pipeline');
  await user.type(screen.getByTestId('agent-description-input'), 'does a thing');
  const save = screen.getByTestId('pipeline-save-button');
  await waitFor(() => expect(save).not.toBeDisabled());
  await user.click(save);
}

/** The shape of the starter graph the assertions below read. */
interface StarterDocument {
  readonly entry_point?: string;
  readonly nodes?: readonly {
    readonly id?: string;
    readonly type?: string;
    readonly transition?: string;
    readonly input_mapping?: Record<string, unknown>;
  }[];
}

/** The `instructions` string the create POST carried — the pipeline's own graph. */
function firstVersionInstructions(bodies: readonly Record<string, unknown>[]): string {
  const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
  const instructions = versions?.[0]?.['instructions'];
  return typeof instructions === 'string' ? instructions : '';
}

/** Opens the model menu and picks a row by its catalogue display name. */
async function chooseModel(user: ReturnType<typeof userEvent.setup>, displayName: string): Promise<void> {
  await user.click(await screen.findByTestId('model-selector-name'));
  await user.click(await screen.findByRole('menuitem', { name: new RegExp(displayName) }));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get('*/configurations/models/:projectId', () => HttpResponse.json(CATALOGUE)));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('CreatePipeline', () => {
  it('renders the Save/Cancel tab bar and a form panel containing the real fields', async () => {
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    expect(await screen.findByTestId('pipeline-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();

    // `toBeInTheDocument()` on the panel alone is what let this page ship a
    // self-closing `<Box data-testid="create-pipeline-form-panel" />` with a
    // green unit test — an empty div is in the document. The E2E journey (J16)
    // caught it only because a real browser cannot SEE an empty box. Assert on
    // the fields the panel is supposed to contain, so the container can never
    // again pass while hollow.
    const panel = screen.getByTestId('create-pipeline-form-panel');
    expect(await screen.findByTestId('agent-name-input')).toBeInTheDocument();
    expect(screen.getByTestId('agent-description-input')).toBeInTheDocument();
    expect(panel).toContainElement(screen.getByTestId('agent-name-input'));
    expect(panel).toContainElement(screen.getByTestId('agent-description-input'));
  });

  it('typing a name and description enables Save (the panel is wired, not just present)', async () => {
    // `delay: null`: userEvent's default inter-keystroke delay re-renders the
    // whole form between characters, and this panel now contains the real
    // `CreateAgentForm` (several MUI accordions) rather than an empty Box. At
    // the default delay the two short strings below took over 5s on CI and
    // timed out. Removing the artificial delay keeps every keystroke — and so
    // every `onFieldChange` round trip this test exists to prove — intact.
    const user = userEvent.setup({ delay: null });
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    // Save is gated on applicationCreationSchema, which requires BOTH fields.
    // Driving the real inputs proves onFieldChange reaches the RHF form —
    // rendering the fields but leaving them unwired would fail here.
    expect(await screen.findByTestId('pipeline-save-button')).toBeDisabled();

    await user.type(screen.getByTestId('agent-name-input'), 'my-pipeline');
    expect(screen.getByTestId('pipeline-save-button')).toBeDisabled();

    await user.type(screen.getByTestId('agent-description-input'), 'does a thing');
    await waitFor(() => expect(screen.getByTestId('pipeline-save-button')).not.toBeDisabled());
  }, 20_000);

  it('clicking Cancel opens a confirm dialog, and confirming navigates back to /pipelines/:tab', async () => {
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await user.click(await screen.findByText('Cancel'));
    await user.click(await screen.findByRole('button', { name: 'Discard' }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/latest'));
  });

  it('the Save button starts disabled (name/description are required and start empty) and never calls the create endpoint', async () => {
    const createSpy = vi.fn(() => ({
      id: '1',
      name: '',
      description: '',
      type: 'interface',
      icon: '',
      owner_id: 'user-1',
      created_at: '2026-01-01T00:00:00Z',
    }));
    server.use(getCreateApplicationMockHandler(createSpy));
    const { router } = renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    const saveButton = await screen.findByTestId('pipeline-save-button');
    await waitFor(() => expect(saveButton).toBeDisabled());

    expect(router.state.location.pathname).toBe('/pipelines/create');
    expect(createSpy).not.toHaveBeenCalled();
  });

  /*
   * This page's save handler used to build the create body from
   * `draftDefaults.versionDetails` and override `conversationStarters`
   * alone, with `extraFields` in neither the body nor the `useCallback`
   * dependency list — so everything typed on the page below the two
   * schema-validated fields was replaced by the empty defaults, with a 201
   * back and no way for the user to tell. Proven red before the fix: this
   * assertion read `expected 25 to be 7`.
   */
  it('sends the step limit typed on the page in the create body, not the draft default', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    const stepLimit = await screen.findByLabelText(/Step limit/i);
    await user.clear(stepLimit);
    await user.type(stepLimit, '7');
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect((versions?.[0]?.['meta'] as Record<string, unknown> | undefined)?.['step_limit']).toBe(7);
  });

  /*
   * The pipelines twin of the agents-page defect: the typed welcome message
   * lived in `extraFields`, was echoed back into the form, and never reached
   * the POST body. Asserted off the body — the page echoing its own state is
   * exactly what made this look saved.
   */
  it('sends the welcome message typed on the create form in the create POST body', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await user.type(await screen.findByTestId('agent-welcome-message-input'), 'Hi there');
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]?.['welcome_message']).toBe('Hi there');
  });

  it('mounts the model picker in the advanced-settings panel, showing the project default', async () => {
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    expect(await screen.findByText('GPT-4o')).toBeVisible();
  });

  it('sends the picked model as llm_settings, with a NUMERIC model_project_id', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const raw: string[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        raw.push(await request.text());
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    await chooseModel(user, 'Qwen 3.5');
    await fillAndSave(user);

    await waitFor(() => expect(raw).toHaveLength(1));
    const body = JSON.parse(raw[0] ?? '{}') as { versions?: Record<string, unknown>[] };
    expect(body.versions?.[0]?.['llm_settings']).toMatchObject({ model_name: 'qwen3.5', model_project_id: 1 });
    // On the raw text, because the worker's `positive_u32` refuses a string
    // and `JSON.parse` would hide the difference from `toMatchObject`.
    expect(raw[0]).toContain('"model_project_id":1');
  });

  it('omits llm_settings entirely when the author never touches the picker', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]).not.toHaveProperty('llm_settings');
  });

  it('arms the unsaved-changes guard when a model is picked and nothing else is touched (#133)', async () => {
    const user = userEvent.setup({ delay: null });
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);

    await chooseModel(user, 'Qwen 3.5');

    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));
  });

  /*
   * DEFECT this pins. `instructions` IS the pipeline's graph — the editor
   * parses the saved string and builds both the YAML pane and the canvas from
   * it — and this page stored `''`. So Create pipeline -> Save opened the
   * editor on a blank document with `End` alone on the canvas, and the
   * pipeline could not run at all: the compiler refuses an empty document, so
   * the first chat turn failed in another process with nothing on screen to
   * explain it.
   *
   * `usePipelineEditorCreate` already carried the starter template, and had a
   * test of its own. That hook serves the CHAT surface. This page is the route
   * a person takes, and it never called it — which is why a green test sat
   * beside a broken screen.
   */
  it('creates a new pipeline with the runnable starter graph, not an empty document', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByTestId('agent-name-input');
    // Nothing is typed into Instructions: this is the create flow exactly as a
    // person performs it — name, description, Save.
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const instructions = firstVersionInstructions(bodies);
    expect(instructions).toBe(PIPELINE_STARTER_TEMPLATE);

    // Asserted on the PARSED document as well as on the constant, so this test
    // still says what the stored graph must contain if the template is ever
    // re-authored: one LLM node, wired to the entry point and to END, with the
    // user's turn mapped in through `task` (the empty `messages` channel is
    // what the SDK refuses on a first turn).
    const document = load(instructions) as StarterDocument;
    const [node] = document.nodes ?? [];
    expect(document.nodes).toHaveLength(1);
    expect(node).toMatchObject({ id: document.entry_point, type: 'llm', transition: 'END' });
    expect(node?.input_mapping?.['task']).toEqual({ type: 'variable', value: 'input' });
  });

  /*
   * DEFECT this pins. The starter graph reached the stored row but never the
   * screen: it was substituted inside Save, so "+ Pipeline" opened on an empty
   * YAML pane and an empty canvas. The person creating the pipeline was shown
   * "you have no graph" while Save was about to write one.
   */
  it('opens with the starter graph already in the instructions editor', { timeout: 20_000 }, async () => {
    const { container } = renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByTestId('agent-name-input');
    const editor = container.querySelector('.cm-content');
    if (!(editor instanceof HTMLElement)) throw new Error('the instructions editor did not render');

    // The editor renders the document line by line, so the text is compared
    // line by line rather than against the raw constant's newlines.
    for (const line of PIPELINE_STARTER_TEMPLATE.trim().split('\n')) {
      expect(editor.textContent).toContain(line.trim());
    }
    // A fresh page is not "unsaved work": the seed is the opening value, so
    // the #133 guard must still read it as clean.
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);
  });

  /*
   * The other half of the same rule: the starter graph is a STARTING POINT,
   * never a replacement for the author's own document. What the editor holds
   * at Save is what gets stored.
   */
  it('stores the author\'s own graph when one is typed, instead of the starter template', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    const { container } = renderPipelinesRoute(<CreatePipeline />, '/pipelines/create', { projectId: '1' });

    await screen.findByTestId('agent-name-input');
    const editor = container.querySelector('.cm-content');
    if (!(editor instanceof HTMLElement)) throw new Error('the instructions editor did not render');
    await user.click(editor);
    await user.type(editor, 'entry_point: MINE');

    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    const stored = versions?.[0]?.['instructions'];
    // Not an equality check on the typed text: the editor now OPENS on the
    // starter graph, so the author's keystrokes land inside that document.
    // What the rule needs is that the stored graph is the edited one.
    expect(stored).toContain('entry_point: MINE');
    expect(stored).not.toBe(PIPELINE_STARTER_TEMPLATE);
  });
});

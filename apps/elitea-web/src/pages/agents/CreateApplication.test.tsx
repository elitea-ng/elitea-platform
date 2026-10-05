import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';

import { getCreateApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { useNavBlockerStore } from '@/widgets/app-shell';

import { CreateApplication } from './CreateApplication';
import { renderAgentsRoute } from './__tests__/testRouter';

installCodeMirrorTestPolyfills();

/**
 * The model catalogue the Advanced-settings picker reads. Served for every
 * test in this file, not just the model ones: the page mounts the picker
 * unconditionally now, and `src/test/setup.ts` runs msw with
 * `onUnhandledRequest: 'error'`.
 *
 * `project_id` is spelled as a string here on purpose — that is what
 * `ConfigModel` declares, while the Go catalogue marshals an int32, and the
 * save body has to carry a JSON number either way.
 */
const CATALOGUE = {
  items: [
    { name: 'gpt-4o', display_name: 'GPT-4o', project_id: '1', default: true },
    { name: 'qwen3.5', display_name: 'Qwen 3.5', project_id: '1' },
  ],
  default_model_name: 'gpt-4o',
};

function serveCatalogue(): void {
  server.use(http.get('*/configurations/models/:projectId', () => HttpResponse.json(CATALOGUE)));
}

/** Opens the model menu and picks a row by its catalogue display name. */
async function chooseModel(user: ReturnType<typeof userEvent.setup>, displayName: string): Promise<void> {
  await user.click(await screen.findByTestId('model-selector-name'));
  await user.click(await screen.findByRole('option', { name: new RegExp(displayName) }));
}

/** Fills the required Name/Description fields so Save becomes enabled, then clicks it. */
async function fillAndSave(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  const saveButton = await screen.findByTestId('agent-save-button');
  await waitFor(() => expect(saveButton).toBeDisabled());

  await user.type(screen.getByTestId('agent-name-input'), 'My Agent');
  await user.type(screen.getByTestId('agent-description-input'), 'A description');
  await waitFor(() => expect(saveButton).not.toBeDisabled());

  await user.click(saveButton);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  serveCatalogue();
});

afterEach(() => {
  resetGeneratedClient();
});

describe('CreateApplication', () => {
  it('renders the Save/Cancel tab bar and the real CreateAgentForm (name/description fields)', async () => {
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    expect(await screen.findByTestId('agent-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
    expect(screen.getByTestId('agent-name-input')).toBeInTheDocument();
  });

  it('typing into the CreateAgentForm name field enables Save once name/description are both filled', async () => {
    const user = userEvent.setup();
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    const saveButton = await screen.findByTestId('agent-save-button');
    await waitFor(() => expect(saveButton).toBeDisabled());

    await user.type(screen.getByTestId('agent-name-input'), 'My Agent');
    await user.type(screen.getByTestId('agent-description-input'), 'A description');

    await waitFor(() => expect(saveButton).not.toBeDisabled());
    // [F2] explicit timeout, not the 5000ms default: two userEvent.type()
    // calls simulate 22 real keystrokes, each round-tripping through React
    // state — legitimately slower than 5s under CI's shared runners when
    // this test lands near the tail of the full, highly-parallel suite run
    // (observed: PR #21's first batch-2 run, real timeout at 5000ms).
  }, 15_000);

  it('clicking Cancel opens a confirm dialog, and confirming navigates back to /agents/:tab', async () => {
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await user.click(await screen.findByText('Cancel'));
    await user.click(await screen.findByRole('button', { name: 'Discard' }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/latest'));
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
    const { router } = renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    const saveButton = await screen.findByTestId('agent-save-button');
    await waitFor(() => expect(saveButton).toBeDisabled());

    expect(router.state.location.pathname).toBe('/agents/create');
    expect(createSpy).not.toHaveBeenCalled();
  });

  it('shows a generic error message when create fails with a non-field-shaped error body', async () => {
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await fillAndSave(user);

    expect(await screen.findByText('Failed to create the agent.')).toBeInTheDocument();
  }, 15_000);

  it('attributes a duplicate-name conflict to the Name field instead of showing one generic message — old app: useCreateApplication.jsx:85-107\'s formik.setFieldError', async () => {
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', () =>
        HttpResponse.json([{ loc: ['body', 'name'], msg: 'An agent with this name already exists.' }], {
          status: 400,
        }),
      ),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await fillAndSave(user);

    expect(await screen.findByText('Name: An agent with this name already exists.')).toBeInTheDocument();
    expect(screen.queryByText('Failed to create the agent.')).not.toBeInTheDocument();
  }, 15_000);

  it('clears the create-error banner once a subsequent save succeeds', async () => {
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await fillAndSave(user);
    await screen.findByText('Failed to create the agent.');

    server.use(
      getCreateApplicationMockHandler({
        id: '1',
        name: 'My Agent',
        description: 'A description',
        type: 'interface',
        icon: '',
        owner_id: 'user-1',
        created_at: '2026-01-01T00:00:00Z',
      }),
    );
    await user.click(screen.getByTestId('agent-save-button'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/latest/1'));
  }, 15_000);

  /*
   * #307 — `CreateAgentForm` now renders the conversation-starters editor
   * itself, so this page had to route the path or the create form would ship
   * the same inert control the edit page was filed for. Asserted off the
   * POST body: the editor keeps no state of its own, so "the text is on
   * screen" would prove only that the page echoed it back.
   */
  // 15s, not the 5s default: this one test types into three fields (starter,
  // name, description) and `user.type` is per-keystroke — it passed alone and
  // timed out when the whole `pages/agents` + `features/agents` suite ran in
  // parallel on this machine.
  it('sends a conversation starter typed on the create form in the create POST body', { timeout: 15_000 }, async () => {
    const user = userEvent.setup();
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    const add = await screen.findByTestId('agent-conversation-starter-add');
    expect(add).toBeVisible();
    await user.click(add);
    await user.type(screen.getByTestId('agent-conversation-starter-input'), 'Ask');

    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]?.['conversation_starters']).toEqual(['Ask']);
  });

  /*
   * The welcome message a user types on CREATE used to be held in
   * `extraFields`, echoed straight back into the form — and dropped from the
   * POST body, which carried no `welcome_message` key at all. The 201 came
   * back, the page navigated to the new agent, and the greeting was gone.
   * Asserted off the body, not off the screen: the page echoing its own
   * state proves nothing about what reached the server, which is precisely
   * how this survived.
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
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await user.type(await screen.findByTestId('agent-welcome-message-input'), 'Hi there');
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]?.['welcome_message']).toBe('Hi there');
  });

  /*
   * The picker's own behaviour is covered in
   * `widgets/agent-model-settings`; what these three pin is the wiring this
   * page owns — that the slot is mounted at all, that the chosen model
   * reaches the create body, and that the nav blocker can see it. A declared
   * slot prop nobody renders is this codebase's recurring defect.
   */
  it('mounts the model picker in the advanced-settings panel, showing the project default', async () => {
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

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
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    await chooseModel(user, 'Qwen 3.5');
    await fillAndSave(user);

    await waitFor(() => expect(raw).toHaveLength(1));
    const body = JSON.parse(raw[0] ?? '{}') as { versions?: Record<string, unknown>[] };
    expect(body.versions?.[0]?.['llm_settings']).toMatchObject({ model_name: 'qwen3.5', model_project_id: 1 });
    // Asserted on the raw text, not the parsed object: `JSON.parse` turns
    // `"1"` and `1` into values that both `toMatchObject` loosely, and the
    // Rust worker parses this field with `positive_u32`, which hard-fails on
    // a string.
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
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    // Merely rendering the catalogue default must not author it — the
    // server-side fallback to that default is what makes an agent with an
    // empty `llm_settings` run at all.
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]).not.toHaveProperty('llm_settings');
  });

  it('arms the unsaved-changes guard when a model is picked and nothing else is touched (#133)', async () => {
    const user = userEvent.setup({ delay: null });
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await screen.findByText('GPT-4o');
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);

    await chooseModel(user, 'Qwen 3.5');

    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));
  });

  /*
   * Variable AUTHORING (this batch). The rows are derived from the
   * instructions rather than typed into a list of their own
   * (`features/agents/model/useCreateAgentFormState.ts`'s
   * `deriveVariablesFromInstructions`), so the only thing that proves an
   * author's placeholder actually becomes a stored variable is the POST body
   * — the page echoing its own `extraFields` back into the panel proves
   * nothing, which is how the welcome-message drop above survived.
   */
  it('sends the variables derived from a placeholder typed into the instructions in the create POST body', { timeout: 30_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post('*/elitea_core/applications/prompt_lib/:projectId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '7', version_details: { id: '1' } }, { status: 201 });
      }),
    );
    renderAgentsRoute(<CreateApplication />, '/agents/create', { projectId: '1' });

    await screen.findByTestId('agent-save-button');
    const editor = await waitFor(() => {
      const content = document.querySelector('.cm-content');
      if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
      return content;
    });
    await user.click(editor);
    // `{{` is user-event's escape for a literal `{`; `}` needs none.
    await user.keyboard('Summarize {{{{topic}} in a {{{{tone}} voice.');

    // The row has to exist on screen before Save, or the assertion below
    // could pass off state the panel never rendered.
    const row = await screen.findByLabelText('topic');
    await user.type(row, 'weather');

    await fillAndSave(user);

    await waitFor(() => expect(bodies).toHaveLength(1));
    const versions = bodies[0]?.['versions'] as Record<string, unknown>[] | undefined;
    expect(versions?.[0]?.['variables']).toEqual([
      { name: 'tone', value: '' },
      { name: 'topic', value: 'weather' },
    ]);
  });
});

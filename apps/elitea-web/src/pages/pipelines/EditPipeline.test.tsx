import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';

import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';
import { NavBlockerDialog, useNavBlockerStore } from '@/widgets/app-shell';

// Deep import into the slice's own store: test files are excluded from
// dependency-cruiser's `no-deep-slice-import` fence, and the store is the
// observable effect of the seeding this page now performs.
import { usePipelineYamlStore } from '@/features/pipelines/model/pipelineYamlStore';

import { EditPipeline } from './EditPipeline';
import { renderPipelinesRoute, renderPipelinesRouteWithoutSocket } from './__tests__/testRouter';

// `ConfigurationTab`'s real `EditorPanel`/`FlowEditor` needs both jsdom
// polyfills this provides (CodeMirror's YAML mode, `ResizeObserver` for
// react-flow's `<ZoomPane>`) to mount successfully instead of falling back
// to `FlowEditorErrorBoundary`'s own fallback — both are real browser
// standards jsdom simply doesn't implement (real browsers always have
// `ResizeObserver`, so this is a jsdom-only gap, not a production one),
// same as `features/pipelines/ui/EditorPanel.test.tsx`/`YamlCodeEditor.test.tsx`
// and every other test file in this worktree that mounts the real flow
// editor or a CodeMirror instance.
installCodeMirrorTestPolyfills();

const globals = globalThis as unknown as Record<string, unknown>;

/** Same fixture shape `lib/isPublicPipelinesProject.test.ts` already establishes for this exact config surface. */
function setPublicProjectId(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

function detail(
  overrides: { versions?: { id: string; name: string; status: string; agent_type: string; created_at: string }[] } = {},
) {
  return {
    id: '42',
    name: 'My Pipeline',
    description: 'A helpful pipeline',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: overrides.versions ?? [
      { id: '1', name: 'base', status: 'draft', agent_type: 'pipeline', created_at: '2026-01-01T00:00:00Z' },
    ],
    version_details: {
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
      agent_type: 'pipeline',
      /*
       * A pipeline's `instructions` IS its graph, so this default has to be a
       * document the compiler would accept — `'Be helpful.'` (an AGENT's
       * shape) parses to a node-less document, which `compiler.rs:459`
       * refuses and the admission gate therefore vetoes Save on. Four
       * save-path tests below were passing only because that veto did not
       * exist yet; they assert on the PUT, not on admission, so the fixture
       * is what was wrong.
       */
      instructions: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      conversation_starters: ['Hi there'],
    },
  };
}

/**
 * The model catalogue the configuration panel's picker reads. Served for
 * every test here — the panel mounts it unconditionally, and
 * `src/test/setup.ts` runs msw with `onUnhandledRequest: 'error'`.
 * `project_id` is spelled as a string because that is what `ConfigModel`
 * declares, while the Go catalogue marshals an int32.
 */
const CATALOGUE = {
  items: [
    { name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true },
    { name: 'qwen3.5', display_name: 'Qwen 3.5', project_id: '9' },
  ],
  default_model_name: 'gpt-4o',
};

/**
 * The configuration-form slot, scoped.
 *
 * Every model-picker query below goes through this rather than `screen`:
 * the chat slot now renders the REAL `ChatBox`, which carries a model
 * selector of its own, so an unscoped `getByText('GPT-4o')` matches two
 * elements. Scoping is also the stronger assertion — it says the picker is
 * in the panel it belongs to, not merely somewhere on the page.
 */
function configPanel() {
  return within(screen.getByTestId('edit-pipeline-configuration-form-gap'));
}

/** Opens the CONFIGURATION panel's model menu and picks a row by its catalogue display name. */
async function chooseModel(user: ReturnType<typeof userEvent.setup>, displayName: string): Promise<void> {
  await user.click(await configPanel().findByTestId('model-selector-name'));
  await user.click(await screen.findByRole('menuitem', { name: new RegExp(displayName) }));
}

beforeEach(() => {
  /*
   * `usePipelineYamlStore` is module-scoped, so without this reset a document
   * one test left behind judges the NEXT test's Save button — and since the
   * admission gate now (correctly) refuses a node-less graph, a leaked one
   * disables Save on a page that never loaded it. Two tests below already
   * reset it by hand for exactly this reason; doing it once here makes the
   * isolation unconditional rather than remembered.
   */
  usePipelineYamlStore.setState({ yamlCode: '', yamlJsonObject: {}, layoutVersion: undefined });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/configurations/models/:projectId', () => HttpResponse.json(CATALOGUE)),
    // The Chat button resolves the signed-in user to add the USER participant
    // (the resolver's author join needs it), so every render of this page now
    // issues the author read. RAW body, no `{data:…}` envelope — `eliteaFetch`
    // wraps the parsed body itself.
    http.get('*/social/author*', () => HttpResponse.json({ id: '6', name: 'E2E Chat Driver', avatar: '' })),
    // The editor's chat pane mounts the real `ChatBox`, whose read-aloud hook
    // asks the project for its TTS voices as soon as a model is selected.
    http.get('*/configurations/tts_voices/*', () => HttpResponse.json({ items: [] })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('EditPipeline', () => {
  // #135 (read half): the standalone editor page never seeded the flow-editor
  // stores, so a stored pipeline's graph was never shown — the canvas always
  // started from an empty document regardless of what the version held.
  it('seeds the flow-editor YAML store from the loaded version instructions', async () => {
    usePipelineYamlStore.setState({ yamlCode: '', yamlJsonObject: {}, layoutVersion: undefined });
    const graphYaml = 'entry_point: Agent 1\nnodes:\n  - id: Agent 1\n    type: llm\n';
    const base = detail();
    const withGraph = {
      ...base,
      version_details: {
        ...base.version_details,
        instructions: graphYaml,
        pipeline_settings: { layout_version: '1.0' },
      },
    };
    server.use(getGetApplicationMockHandler(withGraph));

    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toBe(graphYaml));
    expect(usePipelineYamlStore.getState().layoutVersion).toBe('1.0');
  });

  it('renders the pipeline name once it loads', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    expect(await screen.findByText('My Pipeline')).toBeInTheDocument();
  });

  it('mounts the real ConfigurationTab (GeneralFormPanel + a live EditorPanel/flow editor), not an empty placeholder', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    // `GeneralFormPanel`'s own `data-testid` (`features/pipelines/ui/GeneralFormPanel.tsx`) —
    // proves `ConfigurationTab` itself mounted, not just its disclosed-gap fallback.
    expect(await screen.findByTestId('pipeline-config-tab')).toBeInTheDocument();
    // `EditorPanel`'s own "Add node" trigger — only rendered in its default Flow mode
    // (`EditorPanel.tsx`'s own `mode === PipelineEditorMode.Flow` branch) — proves the
    // real flow editor (not the old unconditional empty
    // `<Box data-testid="edit-pipeline-configuration-tab-panel" />`) is live.
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeInTheDocument();
    // The ONE remaining cross-slice gap (no promoted `features/agents` configuration
    // panels) still shows its disclosed placeholder, not a silently blank area.
    expect(screen.getByTestId('edit-pipeline-configuration-form-gap')).toBeInTheDocument();
    // The chat slot is no longer a gap: it renders the real `widgets/chat-box`
    // `ChatBox` (`./ui/PipelineTestChat.tsx`). `chat-message-input` is that
    // composer's own testid — a placeholder cannot produce it.
    expect(screen.queryByTestId('edit-pipeline-chat-gap')).not.toBeInTheDocument();
    expect(await screen.findByTestId('edit-pipeline-test-chat')).toBeInTheDocument();
    expect(within(screen.getByTestId('edit-pipeline-test-chat')).getByTestId('chat-message-input')).toBeInTheDocument();
  });

  it('contains a render crash in the editor subtree instead of taking the whole page down', async () => {
    /*
     * CORRECTED PREMISE. This test used to describe itself as a guard for
     * "the real app-tree state today — nobody mounts a
     * `SocketClientContext.Provider`". That was false:
     * `src/app/providers/AppProviders.tsx` mounts one around every page, so
     * `useSocketClient()` does NOT throw in production and this fallback is
     * an ERROR path, not the steady state.
     *
     * The scenario is still worth pinning, because what it actually measures
     * is the boundary: a throw anywhere in the editor subtree (the largest in
     * the app) must not unmount the page and take the user's unsaved graph —
     * and the Save button they would keep it with — along with it. A missing
     * socket provider is simply the cheapest way to make that subtree throw
     * on its first render.
     */
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRouteWithoutSocket(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    expect(await screen.findByTestId('edit-pipeline-configuration-tab-error')).toBeInTheDocument();
    // The rest of the page — name, Save/Cancel bar — survives the contained error
    // (the boundary fires on the very first render, before the detail fetch resolves).
    expect(await screen.findByText('My Pipeline')).toBeInTheDocument();
    expect(await screen.findByTestId('pipeline-save-button')).toBeInTheDocument();
  });

  it('shows the not-found state when the URL version is not in the versions list', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42/999', { projectId: '9' });

    expect(await screen.findByText('Version not found')).toBeInTheDocument();
  });

  it('skips the not-found check when isFromCreation=true', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42/999', { projectId: '9' });
    await waitFor(() => expect(screen.getByText('Version not found')).toBeInTheDocument());

    await router.navigate({
      to: '/pipelines/$tab/$agentId/$version',
      params: { tab: 'all', agentId: '42', version: '999' },
      search: { isFromCreation: 'true' },
      replace: true,
    });

    await waitFor(() => expect(screen.getByText('My Pipeline')).toBeInTheDocument());
    expect(screen.queryByText('Version not found')).not.toBeInTheDocument();
  });

  it('renders the Save/Cancel bar once loaded', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    expect(await screen.findByTestId('pipeline-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('clicking Cancel does not throw and keeps the page mounted', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const user = userEvent.setup();
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await screen.findByText('Cancel');
    await user.click(screen.getByText('Cancel'));

    await waitFor(() => expect(screen.getByTestId('pipeline-config-tab')).toBeInTheDocument());
  });

  it('hides the Save/Cancel bar for a read-only viewer of a public pipeline (viewing under the public project)', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/latest/42', { projectId: '42' });

    await screen.findByText('My Pipeline');
    expect(screen.queryByTestId('pipeline-save-button')).not.toBeInTheDocument();
    expect(screen.queryByText('Cancel')).not.toBeInTheDocument();
  });

  it('still renders the Save/Cancel bar for the same pipeline when the selected project is NOT the public project', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    expect(await screen.findByTestId('pipeline-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('shows a dedicated not-found page when the pipeline-detail fetch 404s (e.g. a deleted/nonexistent pipeline)', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/999', { projectId: '9' });

    expect(await screen.findByText('Pipeline not found')).toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-save-button')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-config-tab')).not.toBeInTheDocument();
  });

  it('shows a save-error banner (instead of nothing) when a save attempt fails', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    const saveButton = await screen.findByTestId('pipeline-save-button');
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    await user.click(saveButton);

    expect(await screen.findByText('Failed to save your changes.')).toBeInTheDocument();
  });

  /*
   * The picker rides in `ConfigurationTab`'s configuration-form slot — the
   * left panel, where the baseline puts model settings — so these also pin
   * that it survives alongside the rest of that panel's still-disclosed gap.
   */
  it('mounts the model picker inside the configuration panel, showing the project default', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    const picker = await configPanel().findByText('GPT-4o');
    expect(picker).toBeVisible();
    // Inside the configuration-form slot, not floating somewhere above the
    // editor — the gap notice for the panels that are still missing stays.
    expect(screen.getByTestId('edit-pipeline-configuration-form-gap')).toContainElement(picker);
  });

  it('reads a stored model back onto the picker instead of the project default', async () => {
    const base = detail();
    server.use(
      getGetApplicationMockHandler({
        ...base,
        version_details: {
          ...base.version_details,
          llm_settings: { model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1, temperature: 0.6 },
        },
      }),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    expect(await configPanel().findByText('Qwen 3.5')).toBeVisible();
  });

  it('sends a newly picked model in the version PUT body, with a NUMERIC model_project_id', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    await configPanel().findByText('GPT-4o');
    await chooseModel(user, 'Qwen 3.5');
    await user.click(await screen.findByTestId('pipeline-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const settings = bodies[0]?.['llm_settings'] as Record<string, unknown> | undefined;
    expect(settings?.['model_name']).toBe('qwen3.5');
    expect(settings?.['model_project_id']).toBe(9);
    expect(typeof settings?.['model_project_id']).toBe('number');
  }, 20_000);

  it('leaves llm_settings off the PUT body for a version that names no model and was not re-pointed', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    await configPanel().findByText('GPT-4o');
    await user.click(await screen.findByTestId('pipeline-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    // Rendering the catalogue default must not author it — the omitted key
    // is what leaves the platform's own fallback in charge.
    expect(bodies[0]).not.toHaveProperty('llm_settings');
  }, 20_000);

  it('arms the unsaved-changes guard when only the model is changed (#133)', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    await configPanel().findByText('GPT-4o');
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);

    await chooseModel(user, 'Qwen 3.5');

    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));
  }, 20_000);
});

describe('leaving the editor', () => {
  /**
   * Measured defect (worse than the agents twin's): Discard was
   * `form.reset()` alone, while Save reads the LIVE graph through
   * `usePipelineGraphDraft()` — so a user who edited the canvas, clicked
   * Cancel→Discard, and later clicked Save had the "discarded" edits
   * silently PERSISTED, and stayed on the edit page with no way out.
   * Confirming the discard now reverts the flow-editor stores to their
   * last-loaded snapshot AND leaves for the list, mirroring
   * `pages/agents/EditApplication.tsx`'s `handleDiscarded`.
   */
  it('confirming the discard dialog drops the in-memory draft and navigates back to the pipelines list', async () => {
    const graphYaml = 'entry_point: Agent 1\nnodes:\n  - id: Agent 1\n    type: llm\n';
    const base = detail();
    server.use(
      getGetApplicationMockHandler({
        ...base,
        version_details: { ...base.version_details, instructions: graphYaml },
      }),
    );
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    // Wait for the version seed, then simulate a canvas/YAML edit the way the
    // editor writes one — straight into the store the save path reads.
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toBe(graphYaml));
    // Both halves of the round-trip state, coherently — `EditorPanel`'s own
    // sync effects regenerate `yamlCode` from `yamlJsonObject` when the two
    // disagree, so editing only one would be silently un-edited by the
    // mounted editor rather than by the discard under test.
    usePipelineYamlStore.getState().setYamlJsonObject({ entry_point: 'Edited' });
    usePipelineYamlStore.getState().setYamlCode('entry_point: Edited\n');

    await user.click(await screen.findByText('Cancel'));
    // The dialog's confirm is 'Discard'; the tab bar's own trigger is
    // 'Cancel', so the confirm name is what disambiguates the two.
    await user.click(await screen.findByRole('button', { name: 'Discard' }));

    // The in-memory draft is DROPPED, not just hidden: the store the save
    // path reads is back at the loaded snapshot. This is the assertion that
    // fails without the fix — the old discard left 'entry_point: Edited'
    // live for the next Save.
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toBe(graphYaml));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/pipelines/all');
    });
    // #133 — the discard itself must not be prompted about: the navigation
    // happened, so the app-wide blocker was disarmed first.
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);
  }, 15_000);

  it('dismissing the discard dialog stays on the edit page', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await user.click(await screen.findByText('Cancel'));
    await screen.findByText('Are you sure you want to discard changes?');
    // Two 'Cancel' buttons exist now — the tab bar's own trigger and the
    // dialog's dismiss. The dialog renders in a portal at the END of the
    // body, so the last match is its button.
    const cancels = screen.getAllByRole('button', { name: 'Cancel' });
    await user.click(cancels[cancels.length - 1] as HTMLElement);

    expect(router.state.location.pathname).toBe('/pipelines/all/42');
  }, 15_000);
});

describe('talking to the pipeline', () => {
  /**
   * The page used to offer no way to speak to the pipeline at all (the chat
   * pane is a disclosed gap). The Chat button creates a conversation,
   * attaches THIS pipeline as a participant, and lands in the chat surface.
   * The participant body is the assertion that matters: a pipeline rides as
   * `entity_name: 'application'` behind the `agent_type: 'pipeline'`
   * discriminator (`features/chat-participants/lib/helpers.ts`'s
   * `buildNonModelParticipant`), and `entity_settings.version_id` is what
   * the resolver joins the version through — a participant without it
   * answers 422 on every turn. The USER entry is load-bearing for the same
   * reason: pipelines resolve through the same `agent_chat.sql` whose author
   * join is an INNER JOIN on it.
   */
  it('the Chat button creates a conversation with the pipeline attached and navigates to it', async () => {
    const participantBodies: unknown[] = [];
    server.use(
      getGetApplicationMockHandler(detail()),
      // RAW bodies, no `{data:…}` envelope: `eliteaFetch` wraps the parsed
      // body itself (`mutator.ts` returns `{data: result.data, …}`), so a
      // mock that pre-wraps lands DOUBLE-wrapped and the client reads
      // `conversation.id === undefined` — the #132 shape, from the other side.
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () =>
        HttpResponse.json({ id: '7', name: 'My Pipeline' }, { status: 201 }),
      ),
      http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request, params }) => {
        participantBodies.push({ conversationId: String(params['conversationId']), body: await request.json() });
        return HttpResponse.json([], { status: 200 });
      }),
    );
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await user.click(await screen.findByTestId('chat-with-pipeline-button', {}, { timeout: 5_000 }));

    await waitFor(() => {
      expect(participantBodies).toHaveLength(1);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/chat/7');
    });
    const captured = participantBodies[0] as { conversationId: string; body: readonly Record<string, unknown>[] };
    expect(captured.conversationId).toBe('7');
    // TWO entries, user first: nothing server-side creates the user mapping
    // on the REST path, and the resolver's author join refuses a conversation
    // without it — the same pair the adhoc send posts. The id rides as a
    // NUMBER, which is what the join's `entity_meta->>'id'` comparison needs.
    expect(captured.body).toHaveLength(2);
    expect(captured.body[0]).toMatchObject({ entity_name: 'user', entity_meta: { id: 6 } });
    expect(captured.body[1]).toMatchObject({
      entity_name: 'application',
      entity_meta: { id: '42', project_id: '9' },
      entity_settings: { version_id: '1', agent_type: 'pipeline' },
    });
  }, 15_000);

  it('a failed conversation create surfaces an error and stays on the page', async () => {
    server.use(
      getGetApplicationMockHandler(detail()),
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () => HttpResponse.json({}, { status: 500 })),
    );
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    await user.click(await screen.findByTestId('chat-with-pipeline-button', {}, { timeout: 5_000 }));

    expect(await screen.findByText('Failed to open a chat with this pipeline.')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/pipelines/all/42');
  }, 15_000);

  /**
   * Audit (handoff brief): TanStack Router's `navigate()` promise resolves
   * only once the actual history push commits — traced through
   * `@tanstack/router-core`'s `commitLocation` (the returned
   * `commitLocationPromise` only resolves from inside `load()`, reached via
   * the history subscriber `Transitioner` installs) and `@tanstack/history`'s
   * `tryNavigation` (a blocked attempt — `blockerFn` resolves `true` — calls
   * `onBlocked` and returns WITHOUT ever calling `task()`, so `notify()`
   * never fires and the load/subscriber path that resolves the promise never
   * runs). `useBlocker`'s own `blockerFnComposed` (`@tanstack/react-router`)
   * is what resolves `true` on Cancel: `NavBlockerDialog`'s `reset()` calls
   * `resolve(true)` inside that awaited promise. Net effect, verified by
   * running this exact test against the unfixed button: clicking Chat while
   * the guard is armed and then Cancelling the dialog left `await
   * navigate(...)` pending forever — `isStarting` never went back to
   * `false`, and the button was stuck on "Opening chat…" until the page was
   * unmounted. Independent of load; a plain synchronous click reproduces it.
   */
  it('recovers the button instead of hanging on "Opening chat…" when the nav-blocker dialog is cancelled', async () => {
    server.use(
      getGetApplicationMockHandler(detail()),
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () =>
        HttpResponse.json({ id: '7', name: 'My Pipeline' }, { status: 201 }),
      ),
      http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', () =>
        HttpResponse.json([], { status: 200 }),
      ),
    );
    const user = userEvent.setup();
    // `NavBlockerDialog` isn't part of this fixture's route tree (the real
    // mount point is `AppShell`) — mounted alongside `EditPipeline` here so
    // the guard this page arms actually has a consumer to block against,
    // same as the app's real composition.
    const { router } = renderPipelinesRoute(
      <>
        <EditPipeline />
        <NavBlockerDialog />
      </>,
      '/pipelines/all/42',
      { projectId: '9' },
    );

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    await configPanel().findByText('GPT-4o');
    await chooseModel(user, 'Qwen 3.5');
    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));

    await user.click(await screen.findByTestId('chat-with-pipeline-button', {}, { timeout: 5_000 }));

    // The conversation + participant IS created — the Chat action's own work
    // completed; only the navigation itself is what the guard intercepts.
    await waitFor(() => expect(screen.getByTestId('nav-blocker-dialog')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByTestId('nav-blocker-dialog')).not.toBeInTheDocument());

    // Still on the edit page — the guard did its job.
    expect(router.state.location.pathname).toBe('/pipelines/all/42');
    // The button must recover rather than stay wedged on "Opening chat…"
    // forever. A short window is enough: nothing further is ever scheduled
    // to resolve the old code's stuck state, so if this hasn't flipped by
    // now it never will.
    await waitFor(() => expect(screen.getByTestId('chat-with-pipeline-button')).toBeEnabled(), { timeout: 2_000 });
    expect(screen.getByTestId('chat-with-pipeline-button')).toHaveTextContent('Chat');
  }, 15_000);
});

/**
 * The version bar (dropdown + Set-as-default + Delete version + Save As
 * Version), asserted AT THE COMPOSITION ROOT rather than only through
 * `lib/usePipelineVersionControls.test.tsx`.
 *
 * That split is the point. The hook's own tests hand it a stub
 * `readGraphDraft` and a hand-built `control`; both halves can be perfectly
 * correct while the page wires neither — which is exactly how this codebase
 * has repeatedly shipped components that were "ported, tested and imported by
 * nothing" (#134/#307/#345, and #597 where 2,475 green unit tests could not
 * see a clobbered ref). Every test below drives the REAL page: the real
 * `useFormContext`, the real `usePipelineGraphDraft` reading the real
 * flow-editor stores, and the real `features/agents` components.
 */
describe('pipeline versioning', () => {
  const twoVersions = [
    { id: '1', name: 'base', status: 'draft', agent_type: 'pipeline', created_at: '2026-01-01T00:00:00Z' },
    { id: '2', name: 'v1', status: 'draft', agent_type: 'pipeline', created_at: '2026-02-01T00:00:00Z' },
  ];

  /** Open the version dropdown and pick a row by its displayed label. */
  async function pickVersion(user: ReturnType<typeof userEvent.setup>, label: string | RegExp): Promise<void> {
    await user.click(await screen.findByTestId('version-selector-trigger'));
    await user.click(await screen.findByRole('menuitem', { name: label }));
  }

  it('mounts the version bar on the editor page', async () => {
    server.use(getGetApplicationMockHandler(detail({ versions: twoVersions })));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    // The dropdown shows the open version, and the write affordance is there.
    expect(await screen.findByTestId('version-selector-trigger')).toHaveTextContent('base');
    expect(await screen.findByRole('button', { name: 'Save As Version' })).toBeVisible();
  }, 20_000);

  /**
   * The acceptance for "a version selector that loads a chosen version's
   * graph into the editor". The navigation is the mechanism, but navigating
   * is not the outcome — the outcome is that the FLOW EDITOR now holds the
   * chosen version's document. Asserting the store (which
   * `usePipelineVersionSync` seeds from the newly-fetched version) is the
   * only assertion that distinguishes the two.
   */
  it('choosing a version loads THAT version graph into the flow editor', async () => {
    /*
     * Asserted with `toContain` on the distinguishing NODE ID rather than byte
     * equality with the seed string. The live editor re-dumps the document it
     * holds (`dumpYaml`), so an exact-match assertion here passes or fails on
     * formatting — measured directly: it was green running this file alone and
     * red under the full `src/features/pipelines` + `src/pages/pipelines` run,
     * which is a flake, not a finding. Which node ids are present is the real
     * claim: it is what tells "the editor loaded the other version" apart from
     * "the trigger relabelled itself".
     */
    const baseYaml = 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n';
    const v2Yaml = 'entry_point: Printer_2\nnodes:\n  - id: Printer_2\n    type: printer\n    transition: END\n';
    usePipelineYamlStore.setState({ yamlCode: '', yamlJsonObject: {}, layoutVersion: undefined });
    const base = detail({ versions: twoVersions });
    server.use(
      getGetApplicationMockHandler({ ...base, version_details: { ...base.version_details, instructions: baseYaml } }),
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/2', () =>
        HttpResponse.json({
          id: '2',
          application_id: '42',
          name: 'v1',
          status: 'draft',
          agent_type: 'pipeline',
          instructions: v2Yaml,
        }),
      ),
    );
    const { router } = renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByTestId('version-selector-trigger');
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toContain('Printer_1'));

    await pickVersion(user, /v1/);

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/all/42/2'));
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toContain('Printer_2'));
    expect(usePipelineYamlStore.getState().yamlCode).not.toContain('Printer_1');
  }, 20_000);

  /**
   * "Save as a new version" end to end, through the page's own composition.
   *
   * Two assertions no stub could satisfy. The POST body must pin
   * `agent_type: 'pipeline'` and carry `meta` — omit either and the created
   * row is an `openai` agent with a reset step limit (see
   * `lib/editPipelineMappers.ts`). And the follow-up PUT must carry a
   * `pipeline_settings.nodes` array derived from the LIVE editor stores by
   * the REAL `usePipelineGraphDraft`, aimed at the id the POST minted —
   * because `CreateVersion` cannot store the graph geometry at all.
   */
  it('saves a new version, then carries the live graph onto it', async () => {
    const graphYaml = 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n';
    const base = detail({ versions: twoVersions });
    const posts: Record<string, unknown>[] = [];
    const puts: { url: string; body: Record<string, unknown> }[] = [];
    server.use(
      getGetApplicationMockHandler({
        ...base,
        version_details: { ...base.version_details, instructions: graphYaml },
      }),
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', async ({ request }) => {
        posts.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '3', application_id: '42', name: 'v2', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        puts.push({ url: request.url, body: (await request.json()) as Record<string, unknown> });
        return HttpResponse.json({ id: '3', application_id: '42', name: 'v2', status: 'draft' });
      }),
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/3', () =>
        HttpResponse.json({ id: '3', application_id: '42', name: 'v2', status: 'draft', agent_type: 'pipeline', instructions: graphYaml }),
      ),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    // Wait for the real editor stores to hold this version's graph — the
    // reader returns `undefined` until they do, and the carry is skipped.
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toContain('Printer_1'));

    await user.click(await screen.findByRole('button', { name: 'Save As Version' }));
    const dialog = within(await screen.findByRole('dialog'));
    // A name the pipeline does not already have — `SaveNewVersionButton`
    // refuses a duplicate client-side before sending anything.
    await user.type(dialog.getByLabelText('Version name'), 'v2');
    await user.click(dialog.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.['name']).toBe('v2');
    expect(posts[0]?.['agent_type']).toBe('pipeline');
    expect(posts[0]?.['meta']).toEqual({ step_limit: 25, internal_tools: [] });

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]?.url).toContain('/version/prompt_lib/9/42/3');
    // The node id, not the seed string byte-for-byte — the editor re-dumps the
    // document it holds, and the claim under test is that the LIVE document
    // travelled, not that its formatting survived.
    expect(puts[0]?.body['instructions']).toContain('Printer_1');
    const settings = puts[0]?.body['pipeline_settings'] as Record<string, unknown>;
    // Laid out by the real `doLayout` off the real store — an empty array is
    // what a page that never reached the flow editor would send.
    expect(Array.isArray(settings['nodes'])).toBe(true);
    expect((settings['nodes'] as unknown[]).length).toBeGreaterThan(0);
  }, 30_000);

  /**
   * `Set as default` (#147) comes with the reused bar rather than being
   * rebuilt — it is the one mutation `AgentVersionControls` owns. Proven by
   * the PATCH actually leaving, because the current default is not READABLE
   * (the 2-segment `GET /default_version/...` the router serves is
   * deliberately absent from the contract, `api/openapi/v2.yaml:7548-7553`),
   * so nothing on screen can stand in for the request.
   */
  it('pins the open version as the default through the version menu', async () => {
    const patched: string[] = [];
    server.use(
      getGetApplicationMockHandler(detail({ versions: twoVersions })),
      http.patch('*/elitea_core/default_version/prompt_lib/:projectId/:applicationId/:versionId', ({ request }) => {
        patched.push(request.url);
        return HttpResponse.json({ ok: true });
      }),
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/2', () =>
        HttpResponse.json({ id: '2', application_id: '42', name: 'v1', status: 'draft', agent_type: 'pipeline', instructions: 'nodes: []\n' }),
      ),
    );
    // Opened on `v1`, not on `base`: `isSetDefaultDisabled` deliberately
    // refuses to pin `base` while no default is recorded, because `base` IS
    // the fallback in that state (`entities/version/model/selectors.ts:60`).
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42/2', { projectId: '9' });
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('version-selector-trigger'));
    await user.click(await screen.findByTestId('agent-version-set-default'));
    await user.click(await screen.findByRole('button', { name: 'Set as a default' }));

    await waitFor(() => expect(patched).toHaveLength(1));
    expect(patched[0]).toContain('/default_version/prompt_lib/9/42/2');

    // The marker lives on the dropdown's own row, so the menu has to be
    // reopened to see it. It is REMEMBERED, not fetched: no response this app
    // can read reports a default (see `AgentVersionControls`' disclosed gap),
    // which is precisely why the PATCH above is the load-bearing assertion
    // and this one is the corroborating half.
    await user.click(await screen.findByTestId('version-selector-trigger'));
    expect(await screen.findByTestId('agent-version-default-marker')).toBeVisible();
  }, 20_000);

  /**
   * Saving while a `:version` route is open must re-seed the editor.
   *
   * `useEditPipelineData` serves `activeVersion` off the application DETAIL on
   * `/pipelines/:tab/:id`, but off a SEPARATE `getApplicationVersionDetail`
   * query — a different cache key — once the URL carries a version. The
   * after-save refetch invalidated only the first, so on any non-default
   * version a save left `initYamlCode` frozen at the pre-edit document: the
   * editor stayed permanently "dirty", the test chat stayed closed behind
   * "Save the pipeline to test it", and the unsaved-changes guard prompted on
   * every navigation — including the next version switch, about changes that
   * were already on the server. Measured on the live stack, where it blocked
   * the switch outright.
   *
   * Latent until this unit: before the selector existed, nothing in the app
   * ever navigated to a `:version` route.
   */
  it('refetches the OPEN version after a save, not just the application detail', async () => {
    const versionGets: string[] = [];
    const puts: Record<string, unknown>[] = [];
    server.use(
      getGetApplicationMockHandler(detail({ versions: twoVersions })),
      http.get('*/elitea_core/version/prompt_lib/:projectId/:applicationId/2', ({ request }) => {
        versionGets.push(request.url);
        return HttpResponse.json({
          id: '2',
          application_id: '42',
          name: 'v1',
          status: 'draft',
          agent_type: 'pipeline',
          // A real one-node graph, not `nodes: []`: this test asserts on the
          // post-save REFETCH, and an empty node list is a document the
          // runtime refuses, so the admission gate keeps Save disabled and
          // the save never happens.
          instructions: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
        });
      }),
      // Delayed on purpose. `useRefetchPipelineAfterSave` fires on the save's
      // completion EDGE — `isSaving` going true and then false across two
      // renders. An msw handler that answers inside the same React batch never
      // produces that edge, so an instant PUT would make this test green
      // whatever the hook does. (That is a real fragility in the hook's
      // trigger, not only in the test; noted, out of this unit's scope.)
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        puts.push((await request.json()) as Record<string, unknown>);
        await delay(30);
        return HttpResponse.json({ id: '2', application_id: '42', name: 'v1', status: 'draft' });
      }),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42/2', { projectId: '9' });
    const user = userEvent.setup();

    await waitFor(() => expect(versionGets.length).toBeGreaterThan(0));
    const before = versionGets.length;

    const saveButton = await screen.findByTestId('pipeline-save-button');
    await waitFor(() => expect(saveButton).toBeEnabled());
    await user.click(saveButton);
    await waitFor(() => expect(puts).toHaveLength(1));

    await waitFor(() => expect(versionGets.length).toBeGreaterThan(before));
  }, 20_000);

  /**
   * Old app `ApplicationTabBar.jsx:65`: a public-project viewer keeps the
   * version list and loses only the write affordances. Rendering the bar
   * outside this page's writer-only block is what makes the first half true;
   * `canSaveNewVersion` is what makes the second half true.
   */
  it('keeps the selector but withholds Save As Version from a public-project viewer', async () => {
    setPublicProjectId('9');
    server.use(getGetApplicationMockHandler(detail({ versions: twoVersions })));
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });

    expect(await screen.findByTestId('version-selector-trigger')).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Save As Version' })).toBeNull();
  }, 20_000);
  /*
   * J16b's cause. LAST in this file on purpose: it is the only test here that
   * drives a save AND the refetch that follows it, and it therefore leaves the
   * editor in the state a real author is in after saving — which the earlier
   * tests, sharing this file's module-scoped editor stores, are not written
   * for. `useRefetchPipelineAfterSave` refetches the detail after
   * every save, and the header row used to be gated on the raw `isFetching`,
   * so the version bar — "Save As Version" included — left the document for
   * the length of that request. The journey clicked the button the moment the
   * save landed and waited 30s for an element that was not there.
   *
   * The refetch is held open deliberately: the assertion is about the window
   * WHILE it is in flight, which is the only window in which the defect
   * existed.
   */
  it('keeps the version bar mounted while the post-save refetch is in flight', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    let refetches = 0;
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async () => {
        // Slow enough that the page renders once with the save in flight:
        // `useRefetchPipelineAfterSave` watches for the true->false edge of
        // `isSaving`, and an instantly-answered PUT never produces one.
        await delay(60);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', async () => {
        refetches += 1;
        // Held open only for the FIRST refetch — the window the defect lived
        // in — so nothing is left in flight for the next test.
        if (refetches === 1) await delay(400);
        return HttpResponse.json(detail());
      }),
    );
    renderPipelinesRoute(<EditPipeline />, '/pipelines/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByTestId('edit-pipeline-configuration-form-gap');
    await configPanel().findByText('GPT-4o');
    expect(await screen.findByRole('button', { name: 'Save As Version' })).toBeInTheDocument();

    refetches = 0;
    await user.click(await screen.findByTestId('pipeline-save-button'));

    await waitFor(() => expect(refetches).toBe(1), { timeout: 10_000 });
    expect(screen.getByRole('button', { name: 'Save As Version' })).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-save-button')).toBeInTheDocument();

    // Let the held refetch land while the tree is still mounted. A response
    // that arrives after the test has finished writes the module-scoped
    // editor stores from an unmounted tree, and the NEXT test then starts
    // dirty — the same leak the `beforeEach` reset above exists for.
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).toContain('Printer_1'), {
      timeout: 10_000,
    });
  }, 20_000);
});

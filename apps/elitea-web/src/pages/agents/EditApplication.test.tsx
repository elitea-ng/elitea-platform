import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';

import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';
import { NavBlockerDialog, useNavBlockerStore } from '@/widgets/app-shell';

import { EditApplication } from './EditApplication';
import { renderAgentsRoute } from './__tests__/testRouter';

// This page's real `CreateAgentForm` mounts a CodeMirror-backed field
// (`InstructionsInput`), which needs both stubs `installCodeMirrorTestPolyfills`
// installs — see that file's doc comment. Without it, jsdom throws out of a
// `requestAnimationFrame` callback (`TypeError: textRange(...).getClientRects
// is not a function`) on every render pass after mount, an unhandled
// rejection vitest can report as a failed run independent of the test's own
// assertions. Reproduced on CI shard 10 for the newly added Tools-panel
// tests below, though the same mount runs — and can throw — in every test in
// this file.
installCodeMirrorTestPolyfills();

const globals = globalThis as unknown as Record<string, unknown>;

/** Same fixture shape `lib/isPublicAgentsProject.test.ts` already establishes for this exact config surface. */
function setPublicProjectId(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

function detail(overrides: { versions?: { id: string; name: string; status: string; agent_type: string; created_at: string }[] } = {}) {
  return {
    id: '42',
    name: 'My Agent',
    description: 'A helpful agent',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: overrides.versions ?? [
      { id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
    ],
    version_details: {
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
      instructions: 'Be helpful.',
      conversation_starters: ['Hi there'],
    },
  };
}

/**
 * `detail()` plus one stored tag (#345). The version-detail read emits
 * `{id, name, data}` per tag — the shape `versionTags` builds in
 * services/elitea-main/internal/api/v2/applications/handler.go.
 */
function detailWithTags() {
  const base = detail();
  return {
    ...base,
    version_details: {
      ...base.version_details,
      tags: [{ id: 4, name: 'finance', data: null }],
    },
  };
}

/** `detail()` plus one attached toolkit — `id` (the mapping row) and `tool_id` (the toolkit instance) differ on purpose, see `features/agents/lib/toolRelation.ts`. */
function detailWithTools() {
  const base = detail();
  return {
    ...base,
    version_details: {
      ...base.version_details,
      tools: [{ id: 5, tool_id: 77, entity_type: 'agent', name: 'Github', type: 'github', config: {} }],
      meta: {},
    },
  };
}

/**
 * The model catalogue the Advanced-settings picker reads. Served for every
 * test here — the page mounts the picker unconditionally, and
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

/** Answers both save calls `useSaveVersion` issues, capturing the version PUT's body. */
function captureVersionSave(sink: Record<string, unknown>[]): void {
  server.use(
    http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
      sink.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
    }),
    http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () => HttpResponse.json({ id: '42' }, { status: 201 })),
  );
}

/** Opens the model menu and picks a row by its catalogue display name. */
async function chooseModel(user: ReturnType<typeof userEvent.setup>, displayName: string): Promise<void> {
  await user.click(await screen.findByTestId('model-selector-name'));
  await user.click(await screen.findByRole('menuitem', { name: new RegExp(displayName) }));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/configurations/models/:projectId', () => HttpResponse.json(CATALOGUE)),
    // The Chat button resolves the signed-in user to add the USER participant
    // (the resolver's author join needs it), so every render of this page now
    // issues the author read.
    http.get('*/social/author*', () => HttpResponse.json({ id: '6', name: 'E2E Chat Driver', avatar: '' })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('EditApplication', () => {
  it('renders the application name once it loads', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // 5s, not the 1s default: the configuration panel now renders the real
    // `CreateAgentForm` (several MUI accordions) instead of an empty Box, so the
    // first paint is much heavier. This passed locally and failed on CI at the
    // default timeout, with the DOM showing the fallback `<h3>Agent</h3>` — the
    // query had simply not resolved yet. The assertion is unchanged; only the
    // wait is realistic for a slower machine.
    //
    // The query's own 5s timeout used to equal vitest's 5s default test
    // budget — a slow-but-correct mount and an uninformative "Test timed
    // out" raced for the same clock. Scoped to 15s below, same pattern as
    // the rename test, so the query still fails with a named element when
    // one is genuinely missing, well before the test budget would.
    expect(await screen.findByText('My Agent', {}, { timeout: 5_000 })).toBeInTheDocument();
  }, 15_000);

  it('renders the configuration tab panel with the real agent fields in it', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // Asserting the panel is `toBeInTheDocument()` is what let this page ship a
    // self-closing `<Box data-testid=… />` for so long — an empty div is in the
    // document. Assert it CONTAINS the fields, so a hollow panel fails here
    // rather than waiting for an E2E journey to notice.
    // Both queries below carry the same 5s timeout as vitest's 5s default
    // test budget, so a scoped 15s budget (the pattern the rename test
    // fixed first) is needed here too — see that test's comment for why an
    // equal timeout/budget pair flakes on a slow-but-correct CI run.
    const panel = await screen.findByTestId('edit-application-configuration-tab-panel', {}, { timeout: 5_000 });
    expect(await screen.findByTestId('agent-name-input', {}, { timeout: 5_000 })).toBeInTheDocument();
    expect(panel).toContainElement(screen.getByTestId('agent-name-input'));
    expect(panel).toContainElement(screen.getByTestId('agent-description-input'));
  }, 15_000);

  it('shows the not-found state when the URL version is not in the versions list', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42/999', { projectId: '9' });

    // The not-found state renders only after the detail fetch settles and
    // the version list is resolved; under CI coverage instrumentation that
    // takes longer than Testing Library's 1 s default (shard 2 on 26ad27cc).
    expect(await screen.findByText('Version not found', {}, { timeout: 5_000 })).toBeInTheDocument();
  });

  it('skips the not-found check when isFromCreation=true', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    // Navigated to imperatively (rather than embedding `?isFromCreation=true`
    // directly in the initial memory-history entry) so the query string goes
    // through TanStack Router's own typed `navigate()` — the same API this
    // unit's pages themselves use — instead of relying on how
    // `createMemoryHistory`'s `initialEntries` parses a combined path+query
    // string, which this fixture found does NOT reliably populate
    // `location.search` for a cold `initialEntries` string.
    const { router } = renderAgentsRoute(<EditApplication />, '/agents/all/42/999', { projectId: '9' });
    await waitFor(() => expect(screen.getByText('Version not found')).toBeInTheDocument(), { timeout: 5_000 });

    await router.navigate({
      to: '/agents/$tab/$agentId/$version',
      params: { tab: 'all', agentId: '42', version: '999' },
      search: { isFromCreation: 'true' },
      replace: true,
    });

    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    expect(screen.queryByText('Version not found')).not.toBeInTheDocument();
  });

  it('renders the Save/Cancel bar once loaded', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    expect(await screen.findByTestId('agent-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  /*
   * #134 — the page fetched `versions[]` and spent it exclusively on
   * `useIsVersionNotFound`'s 404 check; nothing on screen ever showed a
   * version. Both of these assert on the MOUNTED control, not on a component
   * existing somewhere in the tree, which is precisely the distinction that
   * the dead `SaveNewVersionButton` (zero importers) slipped through.
   */
  it('mounts the version selector and lists the agent\'s versions', async () => {
    server.use(
      getGetApplicationMockHandler(
        detail({
          versions: [
            { id: '1', name: 'base', status: 'draft', agent_type: 'classic', created_at: '2026-01-01T00:00:00Z' },
            { id: '2', name: 'v1', status: 'draft', agent_type: 'classic', created_at: '2026-01-02T00:00:00Z' },
          ],
        }),
      ),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // Scoped 15s budget: the trigger's own 5s query timeout used to equal
    // vitest's 5s default test budget (the same class the rename test's
    // comment documents), so a slow-but-correct mount could red as "Test
    // timed out" instead of naming a missing element.
    await user.click(await screen.findByTestId('version-selector-trigger', {}, { timeout: 5_000 }));

    const items = await screen.findAllByRole('menuitem');
    // #147 put two COMMAND items ("Set as default", "Delete version") in the
    // same menu; every command item carries a test id, so filtering on its
    // absence keeps this an assertion about the version rows alone.
    const versionRows = items.filter((item) => item.dataset['testid'] === undefined);
    expect(versionRows.map((item) => item.textContent)).toEqual([
      expect.stringContaining('base'),
      expect.stringContaining('v1'),
    ]);

    /*
     * #147 — and the set-default item must be reachable FROM THIS PAGE, not
     * merely exported. JRNY-015's middle step had a route, a handler, a repo
     * write and a generated client, and no mount point; asserting it only in
     * `features/agents`' own test would have gone green against exactly the
     * gap the issue reports.
     */
    expect(screen.getByTestId('agent-version-set-default')).toBeInTheDocument();
  }, 15_000);

  it('mounts "Save As Version" for an owner and withholds it from a read-only viewer', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const owner = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    // Two full mounts in this one test, each carrying a 5s query timeout
    // that used to equal vitest's 5s default test budget. Scoped 15s budget
    // below gives both mounts room, same pattern the rename test fixed.
    expect(await owner.findByRole('button', { name: /save as version/i }, { timeout: 5_000 })).toBeInTheDocument();
    owner.unmount();

    setPublicProjectId('9');
    const viewer = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    await viewer.findByTestId('version-selector-trigger', {}, { timeout: 5_000 });
    expect(viewer.queryByRole('button', { name: /save as version/i })).not.toBeInTheDocument();
  }, 15_000);

  it('clicking Cancel does not throw and keeps the page mounted', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const user = userEvent.setup();
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await screen.findByText('Cancel');
    await user.click(screen.getByText('Cancel'));

    await waitFor(() => expect(screen.getByTestId('edit-application-configuration-tab-panel')).toBeInTheDocument());
  });

  it('hides the Save/Cancel bar for a read-only viewer of a public agent (viewing under the public project)', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/latest/42', { projectId: '42' });

    await screen.findByText('My Agent');
    expect(screen.queryByTestId('agent-save-button')).not.toBeInTheDocument();
    expect(screen.queryByText('Cancel')).not.toBeInTheDocument();
  });

  it('still renders the Save/Cancel bar for the same agent when the selected project is NOT the public project', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    expect(await screen.findByTestId('agent-save-button')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('shows a dedicated not-found page when the application-detail fetch 404s (e.g. a deleted/nonexistent agent)', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/999', { projectId: '9' });

    expect(await screen.findByText('Agent not found')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-save-button')).not.toBeInTheDocument();
    expect(screen.queryByTestId('edit-application-configuration-tab-panel')).not.toBeInTheDocument();
  });

  it('shows a save-error banner (instead of nothing) when a save attempt fails', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    const saveButton = await screen.findByTestId('agent-save-button');
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    await user.click(saveButton);

    expect(await screen.findByText('Failed to save your changes.')).toBeInTheDocument();
  });

  /*
   * #307, end to end through the page's REAL composition — the half
   * `lib/useEditApplicationForm.test.tsx`'s payload tests cannot reach.
   * Those drive `applyFieldChange` directly, so they prove the SAVE carries
   * an edit but not that a keystroke ever gets there; the actual reported
   * defect was in between, in `useEditApplicationEditorBridge`'s
   * `if (path !== 'name' && path !== 'description') return`. Typing into the
   * rendered input and reading the request body off the wire is the only
   * assertion that fails if EITHER half regresses.
   */
  it('persists a typed welcome message: the keystroke reaches the version PUT body, not just the screen', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const versionBodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        versionBodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () =>
        HttpResponse.json({ id: '42' }, { status: 201 }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    // 5s query timeout used to equal vitest's 5s default test budget — see
    // the rename test's comment for why that pairing flakes on a
    // slow-but-correct CI run. Scoped 15s budget below fixes it here too.
    const welcomeInput = await screen.findByTestId('agent-welcome-message-input', {}, { timeout: 5_000 });
    // The whole form renders `disabled` while the detail fetch is in flight,
    // and the panel mounts before it settles — typing into it at that point
    // is silently dropped. Wait for a field the response populates, which is
    // the only observable "the agent has loaded" signal on this page.
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await user.type(welcomeInput, 'Hello!');
    // `toHaveValue` alone is exactly the assertion that used to pass against
    // the broken page: `WelcomeMessageInput` keeps its own local mirror, so
    // the text appeared on screen whether or not anything received it.
    expect(welcomeInput).toHaveValue('Hello!');

    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(versionBodies).toHaveLength(1));
    expect(versionBodies[0]?.['welcome_message']).toBe('Hello!');
  }, 15_000);

  it('persists a renamed agent through the application PUT, which the page never called before issue 307', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const applicationBodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 }),
      ),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', async ({ request }) => {
        applicationBodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '42' }, { status: 201 });
      }),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    // 3s, NOT the 5s this used to carry. A query timeout equal to the whole
    // test budget means one slow first paint consumes 100% of it and the
    // failure surfaces as "Test timed out" rather than naming the element that
    // never arrived. Kept well under the scoped budget below so a genuinely
    // missing input still fails as a query error.
    const nameInput = await screen.findByTestId('agent-name-input', {}, { timeout: 3_000 });
    await waitFor(() => expect(nameInput).toHaveValue('My Agent'));

    // paste, not type. `user.type('Renamed Agent')` dispatches 13 keystrokes
    // and re-renders this page's whole accordion tree on each one; the claim
    // under test is that the name reaches the PUT body, which one input event
    // proves just as well. Measured on CI: this test reported 5172ms against
    // the 5000ms default and reddened shard 10, while taking ~900ms locally.
    await user.clear(nameInput);
    await user.click(nameInput);
    await user.paste('Renamed Agent');

    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(applicationBodies).toHaveLength(1));
    expect(applicationBodies[0]?.['name']).toBe('Renamed Agent');
    // A scoped budget rather than a global testTimeout bump: raising the
    // default in vitest.config.ts would hide slowness creep across the whole
    // suite. CI runs this file roughly 7x slower than a laptop.
  }, 15_000);

  /*
   * #307 — the conversation-starters field was the ONE field this page
   * always sent and the ONE field it had no input for: `CreateAgentForm`
   * carried an empty `conversationStartersSlot`. Typing into the now-mounted
   * editor and reading the PUT body is the assertion that fails if either
   * the mount or the `version_details.conversation_starters` routing
   * regresses; `getByTestId(...)` alone would pass against an editor wired
   * to nothing.
   */
  it('persists an edited conversation starter: the keystroke reaches the version PUT body', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const versionBodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        versionBodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () =>
        HttpResponse.json({ id: '42' }, { status: 201 }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    // 5s query timeout used to equal vitest's 5s default test budget — see
    // the rename test's comment for why that pairing flakes on a
    // slow-but-correct CI run. Scoped 15s budget below fixes it here too.
    const starterInput = await screen.findByTestId('agent-conversation-starter-input', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    // Seeded from the fixture — proof the editor READS the version, before
    // anything is proved about writing it.
    expect(starterInput).toBeVisible();
    expect(starterInput).toHaveValue('Hi there');

    await user.type(starterInput, ' friend');
    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(versionBodies).toHaveLength(1));
    expect(versionBodies[0]?.['conversation_starters']).toEqual(['Hi there friend']);
  }, 15_000);

  it('adds a new conversation starter and sends both of them', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const versionBodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        versionBodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () =>
        HttpResponse.json({ id: '42' }, { status: 201 }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    // 5s query timeout used to equal vitest's 5s default test budget — see
    // the rename test's comment for why that pairing flakes on a
    // slow-but-correct CI run. Scoped 15s budget below fixes it here too.
    await screen.findByTestId('agent-conversation-starter-add', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await user.click(screen.getByTestId('agent-conversation-starter-add'));
    await user.type(screen.getAllByTestId('agent-conversation-starter-input')[1]!, 'Second');

    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(versionBodies).toHaveLength(1));
    expect(versionBodies[0]?.['conversation_starters']).toEqual(['Hi there', 'Second']);
  }, 15_000);

  /*
   * #307 — export/delete/version-delete were fully built with zero
   * importers. These assert the mount and its read-only gate; the controls'
   * own behaviour is covered by their own suites.
   */
  it('mounts the export, delete and version-delete controls for a writer', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // 5s query timeout used to equal vitest's 5s default test budget — see
    // the rename test's comment for why that pairing flakes on a
    // slow-but-correct CI run. Scoped 15s budget below fixes it here too.
    expect(await screen.findByRole('button', { name: /export agent/i }, { timeout: 5_000 })).toBeVisible();
    expect(screen.getByRole('button', { name: /delete entity/i })).toBeVisible();
    // #147 moved version-delete INTO the version menu, where the baseline
    // puts it, so the menu has to be opened to see it. That the page still
    // reaches it is the assertion; the item's own rules are covered by
    // `features/agents/ui/AgentVersionControls.test.tsx`.
    await userEvent.click(screen.getByTestId('version-selector-trigger'));
    expect(screen.getByTestId('agent-version-delete')).toBeVisible();
  }, 15_000);

  it('hides the export, delete and version-delete controls from a read-only viewer of a public agent', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/latest/42', { projectId: '42' });

    await screen.findByText('My Agent');
    expect(screen.queryByRole('button', { name: /export agent/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete entity/i })).not.toBeInTheDocument();
    // The version menu still opens for a read-only viewer — it lists the
    // versions — and it must carry no delete item inside it (#147).
    await userEvent.click(screen.getByTestId('version-selector-trigger'));
    expect(screen.queryByTestId('agent-version-delete')).not.toBeInTheDocument();
  });

  /*
   * #307's last piece — the Tools panel. `detail()` above carries no
   * `tools`, so these use their own fixture; the panel's own attach/detach
   * WIRE behaviour is covered in `features/agents/ui/AgentToolsPanel.test.tsx`
   * (the two PATCH assertions), and these two assert the MOUNT and its
   * read-only gate on the real page.
   */
  it('mounts the Tools panel inside the configuration panel, with the version\'s attached tools on screen', async () => {
    server.use(getGetApplicationMockHandler(detailWithTools()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // 5s query timeout used to equal vitest's 5s default test budget — see
    // the rename test's comment for why that pairing flakes on a
    // slow-but-correct CI run. Scoped 15s budget below fixes it here too.
    const card = await screen.findByTestId('agent-toolkit-card', {}, { timeout: 5_000 });
    // Visible, not merely present: the panel this page shipped before #307
    // was a self-closing empty Box that satisfied toBeInTheDocument().
    expect(card).toBeVisible();
    expect(screen.getByText('Github')).toBeVisible();
    expect(screen.getByTestId('edit-application-configuration-tab-panel')).toContainElement(card);
    expect(screen.getByTestId('agent-add-toolkit-button')).toBeVisible();
  }, 15_000);

  /*
   * #345 — the tag control, asserted from the page's REAL composition root.
   *
   * `AgentTagEditor` was fully written and reachable by nobody: an
   * unexported local of `features/agents/ui/ApplicationEditForm.tsx`, a file
   * no page renders. `CreateAgentForm` had declared the `tagsSlot` prop for
   * it all along and this page passed nothing into it, so a stored tag was
   * invisible and a tag edit was impossible. These two tests fail the moment
   * the slot stops being supplied — a component test of `AgentTagEditor`
   * alone would keep passing, which is exactly how it shipped unmounted.
   */
  it('mounts the tag editor in the configuration panel with the version\'s stored tags on screen', async () => {
    server.use(getGetApplicationMockHandler(detailWithTags()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    const panel = await screen.findByTestId('edit-application-configuration-tab-panel', {}, { timeout: 5_000 });
    // The label proves the control itself is mounted; the chip proves it is
    // bound to the version's stored tags rather than rendering empty.
    expect(await screen.findByLabelText('Tags', {}, { timeout: 5_000 })).toBeInTheDocument();
    const chip = await screen.findByText('finance', {}, { timeout: 5_000 });
    expect(chip).toBeVisible();
    expect(panel).toContainElement(chip);
  }, 15_000);

  it('sends a typed tag to the version PUT body, alongside the stored one', async () => {
    server.use(getGetApplicationMockHandler(detailWithTags()));
    const versionBodies: Record<string, unknown>[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        versionBodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', () =>
        HttpResponse.json({ id: '42' }, { status: 201 }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    const tagsInput = await screen.findByLabelText('Tags', {}, { timeout: 5_000 });
    // Same "wait for a field the response populates" gate the welcome-message
    // test documents: the whole form is `disabled` until the detail settles.
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await user.type(tagsInput, 'newtag{Enter}');

    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(versionBodies).toHaveLength(1));
    // The whole list reaches the wire, not just the edit: the handler
    // REPLACES the association rows, so a body carrying only the new name
    // would silently delete the stored one.
    expect(versionBodies[0]?.['tags']).toEqual([
      { id: 4, name: 'finance' },
      { name: 'newtag' },
    ]);
  }, 15_000);

  it('offers a read-only viewer neither tool control (no attach menu, remove disabled)', async () => {
    setPublicProjectId('42');
    server.use(getGetApplicationMockHandler(detailWithTools()));
    renderAgentsRoute(<EditApplication />, '/agents/latest/42', { projectId: '42' });

    // Same scoped-budget fix as the sibling test above.
    await screen.findByTestId('agent-toolkit-card', {}, { timeout: 5_000 });
    expect(screen.queryByTestId('agent-add-toolkit-button')).not.toBeInTheDocument();
    expect(screen.getByTestId('agent-toolkit-delete-button')).toBeDisabled();
  }, 15_000);

  /*
   * The picker's own behaviour lives in `widgets/agent-model-settings`; what
   * these pin is this page's wiring — the slot is mounted, a stored model
   * comes back on load, a picked one reaches the version PUT, and the nav
   * blocker can see the change. A slot prop that is declared and never
   * rendered is this codebase's recurring defect (#126/#129/#134).
   */
  it('mounts the model picker, showing the project default for a version that pins none', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    expect(await screen.findByText('GPT-4o', {}, { timeout: 5_000 })).toBeVisible();
  }, 15_000);

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
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    expect(await screen.findByText('Qwen 3.5', {}, { timeout: 5_000 })).toBeVisible();
  }, 15_000);

  it('sends a newly picked model in the version PUT body, with a NUMERIC model_project_id', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const bodies: Record<string, unknown>[] = [];
    captureVersionSave(bodies);
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByText('GPT-4o', {}, { timeout: 5_000 });
    // The whole form renders `disabled` until the detail fetch settles, so a
    // click before then is dropped — wait for a field the response populates.
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await chooseModel(user, 'Qwen 3.5');
    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const settings = bodies[0]?.['llm_settings'] as Record<string, unknown> | undefined;
    expect(settings?.['model_name']).toBe('qwen3.5');
    expect(settings?.['model_project_id']).toBe(9);
    expect(typeof settings?.['model_project_id']).toBe('number');
  }, 20_000);

  it('leaves llm_settings off the PUT body for a version that names no model and was not re-pointed', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const bodies: Record<string, unknown>[] = [];
    captureVersionSave(bodies);
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByText('GPT-4o', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await user.click(await screen.findByTestId('agent-save-button'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    // Rendering the catalogue default must not author it: an empty
    // `llm_settings` is what leaves the platform's own fallback in charge,
    // and that fallback is why agent chat works today.
    expect(bodies[0]).not.toHaveProperty('llm_settings');
  }, 20_000);

  it('arms the unsaved-changes guard when only the model is changed (#133)', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });
    const user = userEvent.setup();

    await screen.findByText('GPT-4o', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);

    await chooseModel(user, 'Qwen 3.5');

    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));
  }, 20_000);

  /*
   * #846 — the Information accordion. `features/agents/ui/
   * ApplicationInformation.tsx` was ported, exported and unit-tested, and its
   * ONLY production caller was `pages/pipelines/ui/
   * EditPipelineConfigurationPanel.tsx`. The agent editor — the screen the
   * baseline writes it for (`ApplicationConfigurationForm.jsx:72`) — never
   * mounted it, so the ids a person needs to wire an external caller were
   * unreachable and 17 legacy UI tests timed out on `agent-information-section`.
   *
   * Asserted from the ROUTE, not by rendering the panel directly: a panel-level
   * test cannot see this defect at all. Both halves were correct; only the
   * composition was missing, which is exactly the class
   * `EditApplicationToolsPanel`/`AgentTagEditor` already record in this file.
   */
  it('mounts the Information section inside the configuration panel, showing the ids the detail read carries', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    const panel = await screen.findByTestId('edit-application-configuration-tab-panel', {}, { timeout: 5_000 });
    const information = await screen.findByTestId('agent-information-section', {}, { timeout: 5_000 });
    // `toContainElement`, not merely "on screen": the defect was a missing
    // mount point in THIS panel, and a section rendered anywhere else in the
    // page would satisfy a bare presence check.
    expect(panel).toContainElement(information);

    // The rows, not just the accordion. An empty accordion is in the document.
    // Both values come from the SAME detail read the rest of the panel is
    // driven by — `id` off the application, `version_details.id` off the open
    // version — which is what makes them the ids an external caller needs.
    // `find`, not `get`: the section mounts with the detail read still in
    // flight (the agent id comes from the URL, the version id from the
    // server), so a synchronous read here passes on the id the URL already
    // knew and never waits for the one the panel is being tested for.
    expect(await within(information).findByText('Version ID:', {}, { timeout: 5_000 })).toBeInTheDocument();
    expect(within(information).getByText('Agent ID:')).toBeInTheDocument();
    expect(within(information).getByTestId('copy-id')).toHaveTextContent('42');
    expect(within(information).getByText('1')).toBeInTheDocument();

    // An agent is not a pipeline: the trigger rows and the "Show pipeline"
    // link stay off, and no `pipeline_trigger` request is made (msw runs with
    // `onUnhandledRequest: 'error'`, so one would fail this test outright).
    expect(within(information).queryByText('Trigger:')).not.toBeInTheDocument();
    expect(within(information).queryByText('Pipeline:')).not.toBeInTheDocument();
  }, 15_000);

  it('names the origin agent in the Information section when the open version was forked', async () => {
    const forked = detail();
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', ({ params }) =>
        // The fork's ORIGIN is a different application in a different project.
        // Answering both reads from one handler is what proves the panel asks
        // for the parent at all: a panel that passed its own ids would read
        // back "My Agent" and fail the assertion below.
        params['applicationId'] === '7'
          ? HttpResponse.json({ id: '7', name: 'Origin Agent', versions: [], version_details: { id: '3', application_id: '7', name: 'base', status: 'draft' } })
          : HttpResponse.json({
              ...forked,
              version_details: { ...forked.version_details, meta: { parent_project_id: '5', parent_entity_id: '7' } },
            }),
      ),
    );
    renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    const information = await screen.findByTestId('agent-information-section', {}, { timeout: 5_000 });
    expect(await within(information).findByText('Forked from:', {}, { timeout: 5_000 })).toBeInTheDocument();
    expect(await within(information).findByText('Origin Agent', {}, { timeout: 5_000 })).toBeInTheDocument();
  }, 15_000);
});

describe('leaving the editor', () => {
  /**
   * Measured defect: Cancel opened the "discard changes?" dialog, Discard
   * reverted the fields, and the user was STILL on the edit page — the modal
   * closed editing in neither of its two buttons. Confirming the discard now
   * leaves for the list, matching the create page's Cancel.
   */
  it('confirming the discard dialog navigates back to the agents list', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    const description = await screen.findByTestId('agent-description-input', {}, { timeout: 5_000 });
    await user.type(description, ' edited');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    // The dialog's confirm is 'Discard'; the tab bar's own trigger is
    // 'Cancel', so the confirm name is what disambiguates the two.
    await user.click(await screen.findByRole('button', { name: 'Discard' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/agents/all');
    });
    // #133 — the discard itself must not be prompted about: the navigation
    // happened, so the app-wide blocker was disarmed first.
    expect(useNavBlockerStore.getState().isBlockNav).toBe(false);
  }, 15_000);

  it('dismissing the discard dialog stays on the edit page', async () => {
    server.use(getGetApplicationMockHandler(detail()));
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    // The form's inputs render before the detail fetch settles, but the
    // toolbar (and its Cancel) waits for `!isFetching` — await the BUTTON,
    // not a form field, or this races the fetch.
    await user.click(await screen.findByRole('button', { name: 'Cancel' }, { timeout: 5_000 }));
    await screen.findByText('Are you sure you want to discard changes?');
    // Two 'Cancel' buttons exist now — the tab bar's own trigger and the
    // dialog's dismiss. The dialog renders in a portal at the END of the
    // body, so the last match is its button.
    const cancels = screen.getAllByRole('button', { name: 'Cancel' });
    await user.click(cancels[cancels.length - 1] as HTMLElement);

    expect(router.state.location.pathname).toBe('/agents/all/42');
  }, 15_000);
});

describe('talking to the agent', () => {
  /**
   * "How to talk to agent?" — the page used to offer no way at all. The Chat
   * button creates a conversation, attaches THIS agent as a participant, and
   * lands in the chat surface. The participant body is the assertion that
   * matters: `entity_settings.version_id` is what the agent resolver joins
   * the version through (`agent_chat.sql`), and a participant without it
   * answers 422 on every turn.
   */
  it('the Chat button creates a conversation with the agent attached and navigates to it', async () => {
    const participantBodies: unknown[] = [];
    server.use(
      getGetApplicationMockHandler(detail()),
      // RAW bodies, no `{data:…}` envelope: `eliteaFetch` wraps the parsed
      // body itself (`mutator.ts` returns `{data: result.data, …}`), so a
      // mock that pre-wraps lands DOUBLE-wrapped and the client reads
      // `conversation.id === undefined` — the #132 shape, from the other side.
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () =>
        HttpResponse.json({ id: '7', name: 'My Agent' }, { status: 201 }),
      ),
      http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request, params }) => {
        participantBodies.push({ conversationId: String(params['conversationId']), body: await request.json() });
        return HttpResponse.json([], { status: 200 });
      }),
    );
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await user.click(await screen.findByTestId('chat-with-agent-button', {}, { timeout: 5_000 }));

    await waitFor(() => {
      expect(participantBodies).toHaveLength(1);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/chat/7');
    });
    const captured = participantBodies[0] as { conversationId: string; body: readonly Record<string, unknown>[] };
    expect(captured.conversationId).toBe('7');
    // TWO entries, user first: nothing server-side creates the user mapping
    // on the REST path, and the resolver's author join refuses a
    // conversation without it — the same pair the adhoc send posts.
    expect(captured.body).toHaveLength(2);
    expect(captured.body[0]).toMatchObject({ entity_name: 'user', entity_meta: { id: 6 } });
    expect(captured.body[1]).toMatchObject({
      entity_name: 'application',
      entity_meta: { id: '42', project_id: '9' },
      entity_settings: { version_id: '1' },
    });
  }, 15_000);

  it('a failed conversation create surfaces an error and stays on the page', async () => {
    server.use(
      getGetApplicationMockHandler(detail()),
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () => HttpResponse.json({}, { status: 500 })),
    );
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<EditApplication />, '/agents/all/42', { projectId: '9' });

    await user.click(await screen.findByTestId('chat-with-agent-button', {}, { timeout: 5_000 }));

    expect(await screen.findByText('Failed to open a chat with this agent.')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/agents/all/42');
  }, 15_000);

  /**
   * Audit (handoff brief) — same finding as the pipelines twin
   * (`EditPipeline.test.tsx`'s identically-named test, whose doc comment
   * traces the exact TanStack Router/history mechanism): a BLOCKED
   * `navigate()` that the user then CANCELS never commits, so the promise
   * `ChatWithAgentButton` used to `await` never settled either. Measured
   * against the unfixed button: `isStarting` stayed `true` forever the
   * instant Cancel was clicked on the guard's own dialog — "Opening chat…"
   * with no way back short of leaving the page.
   */
  it('recovers the button instead of hanging on "Opening chat…" when the nav-blocker dialog is cancelled', async () => {
    server.use(
      getGetApplicationMockHandler(detail()),
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () =>
        HttpResponse.json({ id: '7', name: 'My Agent' }, { status: 201 }),
      ),
      http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', () =>
        HttpResponse.json([], { status: 200 }),
      ),
    );
    const user = userEvent.setup();
    // `NavBlockerDialog` isn't part of this fixture's route tree (the real
    // mount point is `AppShell`) — mounted alongside `EditApplication` here
    // so the guard this page arms actually has a consumer to block against,
    // same as the app's real composition.
    const { router } = renderAgentsRoute(
      <>
        <EditApplication />
        <NavBlockerDialog />
      </>,
      '/agents/all/42',
      { projectId: '9' },
    );

    await screen.findByText('GPT-4o', {}, { timeout: 5_000 });
    await waitFor(() => expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent'));
    await chooseModel(user, 'Qwen 3.5');
    await waitFor(() => expect(useNavBlockerStore.getState().isBlockNav).toBe(true));

    await user.click(await screen.findByTestId('chat-with-agent-button', {}, { timeout: 5_000 }));

    // The conversation + participant IS created — the Chat action's own work
    // completed; only the navigation itself is what the guard intercepts.
    await waitFor(() => expect(screen.getByTestId('nav-blocker-dialog')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByTestId('nav-blocker-dialog')).not.toBeInTheDocument());

    // Still on the edit page — the guard did its job.
    expect(router.state.location.pathname).toBe('/agents/all/42');
    // The button must recover rather than stay wedged on "Opening chat…"
    // forever. A short window is enough: nothing further is ever scheduled
    // to resolve the old code's stuck state, so if this hasn't flipped by
    // now it never will.
    await waitFor(() => expect(screen.getByTestId('chat-with-agent-button')).toBeEnabled(), { timeout: 2_000 });
    expect(screen.getByTestId('chat-with-agent-button')).toHaveTextContent('Chat');
  }, 15_000);
});

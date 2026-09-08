import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ToolEvents } from '@/entities/toolkit';
import { eventEmitter } from '@/features/toolkits/lib/eventEmitter';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { EditToolkit } from './EditToolkit';
import { renderToolkitsRoute } from './__tests__/testRouter';

// The `github` mock schema below has no `type` field (matches every other
// fixture in this file — `getToolComponent`, `features/toolkits/lib/helpers/
// toolComponent.helpers.ts`, resolves that to `ToolCustom`'s raw-JSON
// editor, a real CodeMirror instance), so both new tests below need the
// same jsdom polyfills `YamlCodeEditor.test.tsx` (a sibling CodeMirror
// consumer) already establishes for mounting one under jsdom.
installCodeMirrorTestPolyfills();

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function mockToolkitRow(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: 'tk-1',
    type: 'github',
    name: 'My GitHub',
    description: '',
    settings: {},
    meta: {},
    created_at: '2026-01-01T00:00:00Z',
    author_id: 1,
    ...overrides,
  };
}

describe('EditToolkit', () => {
  it('fetches the real toolkit detail by id and shows its name as the title', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    expect(await screen.findByText('My GitHub')).toBeInTheDocument();
  });

  /*
   * CORRECTED (#149). This test used to be titled "renders the
   * Configuration/Indexes tabs, and the Indexes panel is a disclosed
   * composition-gap slot" and asserted that a `github` toolkit offers an
   * Indexes tab whose panel is an empty Box. Both halves were wrong, and it
   * was the unit-level twin of the E2E assertion corrected in
   * `e2e/journeys/mcps/mcps.oauth.spec.ts`:
   *
   *  - the baseline hides this tab entirely for a type whose schema offers no
   *    indexing tool (`apps/elitea-ui/src/pages/Toolkits/EditToolkit.jsx:
   *    210-216`), and `github` is exactly such a type — measured against the
   *    live catalogue, its `selected_tools.args_schemas` carries sixteen
   *    tools and not one `IndexesToolsEnum` member;
   *  - the panel is no longer a slot; `IndexesTab` mounts the real container.
   *
   * The replacement is strictly stronger: it pins the tab strip's exact
   * contents in BOTH directions (a type that must not offer the tab, and one
   * that must), where the old test could only ever confirm that some element
   * with that testid existed.
   */
  it('does NOT offer an Indexes tab for a toolkit type whose schema has no indexing tool', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({ github: { properties: { selected_tools: { args_schemas: { create_issue: { type: 'object' }, search_code: { type: 'object' } } } } } }),
      ),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    expect(screen.getByRole('tab', { name: 'Configuration' })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual(['Configuration']);
    });
    expect(screen.queryByTestId('edit-toolkit-indexes-tab-panel')).not.toBeInTheDocument();
  });

  it('offers the Indexes tab for a type whose schema DOES carry an index tool, and mounts a real panel', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [mockToolkitRow({ type: 'artifact', name: 'My Artifacts', settings: { selected_tools: ['index_data'] } })], total: 1 }),
      ),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({ artifact: { properties: { selected_tools: { args_schemas: { index_data: { type: 'object' }, read_artifact: { type: 'object' } } } } } }),
      ),
      http.get('/api/v2/elitea_core/index_meta/prompt_lib/:projectId/:toolkitId', () => HttpResponse.json([])),
    );
    const saveToolkit = vi.fn();
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My Artifacts');
    const indexesTab = await screen.findByRole('tab', { name: 'Indexes' });

    await user.click(indexesTab);

    const panel = await screen.findByTestId('edit-toolkit-indexes-tab-panel');
    // Not merely "the panel element exists" — that is what the empty Box
    // satisfied. These two come from `IndexesList`, i.e. only from a really
    // mounted `IndexesContainer`.
    expect(await within(panel).findByRole('button', { name: 'Add index' })).toBeInTheDocument();
    expect(within(panel).getByText('Still no indexes created')).toBeInTheDocument();
  });

  /**
   * COMPOSITION ROOT — the worker-capability verdict reaches the screen.
   *
   * `services/elitea-main/internal/api/v2/toolkits/type_catalogue.go:232-240`
   * stamps `metadata.unavailable` + `unavailable_reason` on a type the worker
   * cannot import. Only the type CHOOSER read that
   * (`entities/toolkit/model/toolMenu.ts:113`, on the sibling `hidden`), which
   * stops such a type being created NEW and does nothing for the toolkits that
   * already exist — imported, seeded, or created before the capability changed.
   *
   * The fixture is deliberately the SAME index-capable `artifact` toolkit the
   * test above drives, with the verdict added and nothing else changed. So the
   * only difference between the two screens is the metadata block, and this
   * test fails the moment the verdict stops being read — which is the state
   * the whole app was in.
   */
  it('renders the server’s unavailable_reason instead of the indexes UI when the worker cannot run the type', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [mockToolkitRow({ type: 'artifact', name: 'My Artifacts', settings: { selected_tools: ['index_data'] } })], total: 1 }),
      ),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          artifact: {
            properties: { selected_tools: { args_schemas: { index_data: { type: 'object' }, read_artifact: { type: 'object' } } } },
            metadata: { hidden: true, unavailable: true, unavailable_reason: 'the worker does not declare the toolkit class alita_sdk.tools.artifact' },
          },
        }),
      ),
      http.get('/api/v2/elitea_core/index_meta/prompt_lib/:projectId/:toolkitId', () => HttpResponse.json([])),
    );
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My Artifacts');
    await user.click(await screen.findByRole('tab', { name: 'Indexes' }));

    const panel = await screen.findByTestId('edit-toolkit-indexes-tab-panel');
    expect(await within(panel).findByTestId('indexes-unavailable')).toBeInTheDocument();
    expect(within(panel).getByText('the worker does not declare the toolkit class alita_sdk.tools.artifact')).toBeInTheDocument();
    // The two controls the previous test asserts. Their ABSENCE is the point:
    // an Index button that dispatches a run the worker has already refused is
    // the defect, not the fix.
    expect(within(panel).queryByRole('button', { name: 'Add index' })).not.toBeInTheDocument();
    expect(within(panel).queryByText('Still no indexes created')).not.toBeInTheDocument();
  });

  /**
   * COMPOSITION ROOT — a failed index_meta read is not reported as an empty
   * project.
   *
   * The response below is the one the 2026-09-06 production-parity walk
   * actually recorded (validation matrix row F4): the single HTTP failure of
   * the whole session, on a plain READ, naming the missing prerequisite in a
   * sentence written for a human. The screen it produced said "Still no
   * indexes created".
   */
  it('surfaces the index_meta failure and its prerequisite instead of the empty-list placeholder', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [mockToolkitRow({ type: 'artifact', name: 'My Artifacts', settings: { selected_tools: ['index_data'] } })], total: 1 }),
      ),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({ artifact: { properties: { selected_tools: { args_schemas: { index_data: { type: 'object' } } } } } }),
      ),
      http.get('/api/v2/elitea_core/index_meta/prompt_lib/:projectId/:toolkitId', () =>
        HttpResponse.json({ error: 'PGVector configuration is missing for toolkit 1' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My Artifacts');
    await user.click(await screen.findByRole('tab', { name: 'Indexes' }));

    const panel = await screen.findByTestId('edit-toolkit-indexes-tab-panel');
    expect(await within(panel).findByTestId('indexes-list-error', undefined, { timeout: 15_000 })).toBeInTheDocument();
    expect(within(panel).getByText('PGVector configuration is missing for toolkit 1')).toBeInTheDocument();
    expect(within(panel).getByText(/Create a PgVector configuration in Settings/)).toBeInTheDocument();
    expect(within(panel).queryByText('Still no indexes created')).not.toBeInTheDocument();
  });

  it('renders the export/delete action buttons once a toolkit id is known', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    expect(screen.getByLabelText('export toolkit')).toBeInTheDocument();
    expect(screen.getByLabelText('delete entity')).toBeInTheDocument();
  });

  /**
   * Regression: R2. The baseline's `ToolkitsControls.jsx` (lines 55-69)
   * disables its Export/Delete menu items unless the caller holds BOTH the
   * `applications.*` AND `toolkits.*` permission for that action. Before
   * this fix, `EditToolkit.tsx` never passed a `disabled` prop to either
   * button — both default `disabled={false}` (`ExportToolkitButton.tsx`/
   * `DeleteToolkitButton.tsx`), so every caller could click Delete/Export on
   * any toolkit regardless of permissions. These four tests would all have
   * failed pre-fix: the buttons were never disabled no matter what
   * `usePermissionList` returned.
   */
  it('enables Export/Delete when the caller holds every required permission', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      getPermissionListMockHandler([
        { name: PERMISSIONS.applications.export, enabled: true },
        { name: PERMISSIONS.toolkits.export, enabled: true },
        { name: PERMISSIONS.applications.delete, enabled: true },
        { name: PERMISSIONS.toolkits.delete, enabled: true },
      ]),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => expect(screen.getByLabelText('export toolkit')).not.toBeDisabled());
    expect(screen.getByLabelText('delete entity')).not.toBeDisabled();
  });

  it('disables Export when the caller is missing the toolkits.export permission (holds applications.export only)', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      getPermissionListMockHandler([
        { name: PERMISSIONS.applications.export, enabled: true },
        { name: PERMISSIONS.applications.delete, enabled: true },
        { name: PERMISSIONS.toolkits.delete, enabled: true },
      ]),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => expect(screen.getByLabelText('export toolkit')).toBeDisabled());
    expect(screen.getByLabelText('delete entity')).not.toBeDisabled();
  });

  it('disables Delete when the caller is missing the applications.delete permission (holds toolkits.delete only)', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      getPermissionListMockHandler([
        { name: PERMISSIONS.applications.export, enabled: true },
        { name: PERMISSIONS.toolkits.export, enabled: true },
        { name: PERMISSIONS.toolkits.delete, enabled: true },
      ]),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => expect(screen.getByLabelText('delete entity')).toBeDisabled());
    expect(screen.getByLabelText('export toolkit')).not.toBeDisabled();
  });

  it('disables both Export and Delete when the caller holds no relevant permissions at all', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      getPermissionListMockHandler([]),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => expect(screen.getByLabelText('export toolkit')).toBeDisabled());
    expect(screen.getByLabelText('delete entity')).toBeDisabled();
  });

  /**
   * Regression: R3. Before this fix, `ToolkitsControls` (the kebab dropdown
   * the baseline's `EditToolkit.jsx` renders next to Export/Delete) was
   * fully built, tested, and exported from `features/toolkits`, but
   * imported nowhere in the app — a project-wide grep for `<ToolkitsControls`
   * found no call sites outside its own definition/test files. These two
   * tests would both have failed pre-fix: no "More actions" trigger existed
   * anywhere in the rendered page at all.
   */
  it('renders the ToolkitsControls kebab dropdown next to Export/Delete', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    expect(screen.getByRole('button', { name: 'More actions' })).toBeInTheDocument();
  });

  it('copies the current page URL to the clipboard via the kebab\'s real Copy Link item', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow()], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    );
    const saveToolkit = vi.fn();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    fireEvent.click(await screen.findByText('Copy link'));

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(window.location.href));

    // `ControlsDropdown`'s own `handleLeafActivate` closes the menu right
    // after firing `onClick` — the "Copied!" label swap is only visible on
    // the NEXT open.
    fireEvent.click(screen.getByRole('button', { name: 'More actions' }));
    expect(await screen.findByText('Copied!')).toBeInTheDocument();
  });

  it('falls back to "Edit Toolkit" (or "Edit MCP") while the detail is loading / unresolved', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    );
    const saveToolkit = vi.fn();

    renderToolkitsRoute(<EditToolkit isMCP deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await waitFor(() => expect(screen.getByText('Edit MCP')).toBeInTheDocument());
  });

  /**
   * `github`'s mock toolkit-type schema below (like every other fixture in
   * this file) has no `type` field, so `getToolComponent` (`features/
   * toolkits/lib/helpers/toolComponent.helpers.ts`) resolves `ToolCustom`
   * (its raw-JSON `CodeMirror` editor) rather than `ToolBase` — `ToolBase`
   * itself is never reached this way, so its own `NameDescriptionInput`
   * slot never renders. `ToolCustom`'s serialized JSON
   * (`buildInitialJson`, `ToolCustom.tsx:84-89`) reads `editToolDetail.
   * description` DIRECTLY — the exact same field `toEditDetail` (`../
   * EditToolkit.tsx`) now seeds — so its presence/absence in the rendered
   * JSON is an equally real, equally direct proof of this fix (before the
   * fix, `description` was `undefined` on `editToolDetail`, and
   * `JSON.stringify` omits `undefined`-valued keys outright — the
   * `"description"` key wouldn't appear in the DOM at all).
   */
  function getCodeMirrorContent(container: HTMLElement): HTMLElement {
    const content = container.querySelector('.cm-content');
    if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
    return content;
  }

  it('seeds the Description field from the fetched toolkit detail', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow({ description: 'REAL SAVED DESCRIPTION' })], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })),
    );
    const saveToolkit = vi.fn();

    const { container } = renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => expect(getCodeMirrorContent(container)).toHaveTextContent('REAL SAVED DESCRIPTION'));
  });

  it('passes an edited description through to deps.saveToolkit on save (real ToolkitsUpdateToolkit save path)', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tools/prompt_lib/:projectId', () => HttpResponse.json({ rows: [mockToolkitRow({ description: 'REAL SAVED DESCRIPTION' })], total: 1 })),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })),
    );
    const saveToolkit = vi.fn().mockResolvedValue({ id: 'tk-1', type: 'github', name: 'My GitHub' });
    const user = userEvent.setup();

    const { container } = renderToolkitsRoute(<EditToolkit deps={{ saveToolkit }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    const content = getCodeMirrorContent(container);
    await waitFor(() => expect(content).toHaveTextContent('REAL SAVED DESCRIPTION'));

    // Replace the whole JSON document with the same shape but an edited
    // `description` — `ToolCustom`'s own parse effect (`ToolCustom.tsx:
    // 246-258`) round-trips every parsed field, including `description`,
    // back up through `editField`/`setEditToolDetail` on every valid-JSON
    // edit. A single `paste` (not per-keystroke `type`) — CM6's
    // `closeBrackets` extension auto-inserts a matching `}`/`"` per
    // keystroke, which corrupts a character-by-character JSON retype
    // (confirmed live: it left a stray extra `}` in the document).
    const newJson = '{"name":"My GitHub","description":"UPDATED DESCRIPTION","settings":{},"type":"github"}';

    await user.click(content);
    await user.keyboard('{Control>}a{/Control}');
    await user.paste(newJson);
    await waitFor(() => expect(content).toHaveTextContent('UPDATED DESCRIPTION'));

    // `ToolkitsOperationButtons` (`features/toolkits/ui/form/ToolkitForm/
    // ToolkitsOperationButtons.tsx`) is driven entirely through the shared
    // `eventEmitter` — its own doc comment and test suite establish
    // `eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit)` as the real
    // save trigger a click on `ToolkitsTabBar`'s Save button would emit;
    // `EditToolkit`'s own page doesn't compose that tab bar (a disclosed,
    // pre-existing gap, not this unit's fix), so this exercises the same
    // real internal save path directly. The edited description only
    // reaches `ToolkitsOperationButtons`'s own `formValues` closure after
    // `ToolCustom`'s parse effect finishes its `editField` round-trip and
    // the tree re-renders — one or more passive-effect cycles after the
    // CodeMirror view itself already shows the new text — so the emit is
    // retried (a real click on a real Save button would just as validly be
    // a second real click) until the save call actually carries the
    // edited value.
    await waitFor(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
      expect(saveToolkit.mock.calls.at(-1)?.[0]).toMatchObject({ projectId: 'proj-1', toolId: 'tk-1', type: 'github', description: 'UPDATED DESCRIPTION' });
    });
  });
});

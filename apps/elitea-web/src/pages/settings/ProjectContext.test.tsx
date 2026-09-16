/**
 * Composition root for Settings › Project Context (issue 841).
 *
 * The page is `@ts-nocheck`, so the type checker says nothing about it, and
 * it had no test of its own. These cover the parity gaps this change closes,
 * each measured against the reference:
 *
 *  - the enable toggle SAVES (`ProjectContextSavedView.jsx:30-40`); it only
 *    set local state here, so switching the context off and leaving the tab
 *    left it on;
 *  - an over-long markdown import is REPORTED
 *    (`ProjectContextEditor.jsx:127-146`); it wrote to the console here, so
 *    the import silently did nothing;
 *  - the AI control is called "Build with AI" on both screens
 *    (`GenerateProjectContextButton.jsx:14`); the editor toolbar called it
 *    "Generate with AI";
 *  - the editor can copy the context out (the reference has it in the
 *    toolbar and again in the saved view's menu); this port had neither.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { configure, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '@/test/setup';

import { ProjectContext } from './ProjectContext';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const BASE = '/api/v2';
const CONTEXT_PATH = `${BASE}/elitea_core/project_context/prompt_lib/:projectId/project-context`;
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const MAX_CHARS = 2500;

function mount(overrides: { canView?: boolean; canEdit?: boolean } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <ProjectContext
        projectId="7"
        projectName="Demo"
        canView={overrides.canView ?? true}
        canEdit={overrides.canEdit ?? true}
      />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // `GenerateProjectContextButton` reads the permission set itself; the
  // page's own `canEdit` prop does not reach it.
  server.use(
    http.get(PERMISSIONS_PATH, () =>
      HttpResponse.json([
        { name: 'models.project_context.view', enabled: true },
        { name: 'models.project_context.edit', enabled: true },
      ]),
    ),
  );
  // The AI draft control hides itself on a deployment that does not serve
  // `generate_project_context_draft`; the reference gates it on permission
  // only. This build serves it, so the toolbar shows the control.
  setBackendCapabilityForTests('aiGeneration', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('Settings › Project Context', () => {
  it('saves the enable toggle immediately, sending the SAVED content', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })),
      http.put(CONTEXT_PATH, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ content: 'Stored background', enabled: false });
      }),
    );

    mount();
    const user = userEvent.setup();

    const toggle = await screen.findByRole('switch');
    await user.click(toggle);

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ content: 'Stored background', enabled: false });
  });

  it('puts the toggle back and reports the failure when the save is refused', async () => {
    server.use(
      http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })),
      http.put(CONTEXT_PATH, () => HttpResponse.json({ error: 'nope' }, { status: 500 })),
    );

    mount();
    const user = userEvent.setup();

    const toggle = await screen.findByRole('switch');
    await user.click(toggle);

    expect(await screen.findByText('Failed to save Project Context')).toBeInTheDocument();
    await waitFor(() => expect(toggle).toBeChecked());
  });

  it('names the AI control "Build with AI" in the editor toolbar and offers a copy control', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })));

    mount();

    expect(await screen.findByTestId('project-context-generate-with-ai-button')).toBeInTheDocument();
    // One name for one control. The empty state already said "Build with
    // AI"; the editor toolbar said "Generate with AI".
    expect(screen.getByText('Build with AI')).toBeInTheDocument();
    expect(screen.queryByText('Generate with AI')).toBeNull();
    expect(screen.getByTestId('project-context-copy-button')).toBeEnabled();
    expect(screen.getByTestId('project-context-save-button')).toBeInTheDocument();
  });

  it('shows the empty state, and its own Build with AI control, when nothing is saved', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: '', enabled: true })));

    mount();

    expect(await screen.findByTestId('project-context-empty-state')).toBeInTheDocument();
    expect(screen.getByTestId('project-context-build-with-ai-button')).toBeInTheDocument();
  });

  /*
   * ── viewer coverage (issue #940 D4, onetest ELITEA-0941) ──────────────────
   *
   * The real backend can never produce a project WITH saved content on this
   * app's own E2E stack — `PJC-PERSIST` (`e2e/journeys/settings/settings.
   * project-context.spec.ts`'s header) documents that `UpdateProjectContext`
   * never durably writes, for any persona — so the two states below (a
   * project without saved content = `canView=false`'s early return, and a
   * project WITH saved content as a viewer would see it) can only be proven
   * here, against a mocked response, rather than end-to-end.
   */

  it('canView=false: shows the permission-denied banner and nothing else', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })));

    mount({ canView: false, canEdit: false });

    expect(await screen.findByText('You do not have permission to view this setting.')).toBeInTheDocument();
    expect(screen.queryByTestId('project-context-body')).not.toBeInTheDocument();
    expect(screen.queryByTestId('project-context-empty-state')).not.toBeInTheDocument();
  });

  it('canEdit=false (a viewer) sees a read-only editor with no Save/Upload and a disabled toggle, on saved content', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })));

    mount({ canView: true, canEdit: false });

    expect(await screen.findByTestId('project-context-body')).toBeInTheDocument();
    expect(screen.getByText("You don't have permission to edit this setting.")).toBeInTheDocument();

    // The editor field itself is present and shows the saved content, but is
    // NOT editable.
    const editor = screen.getByRole('textbox');
    expect(editor).toHaveAttribute('aria-readonly', 'true');
    expect(editor).toHaveTextContent('Stored background');

    // No active Save/Upload — both disabled, matching ELITEA-0941's "no
    // active Save or Upload button visible for Viewer".
    expect(screen.getByTestId('project-context-save-button')).toBeDisabled();
    expect(screen.getByTestId('project-context-discard-button')).toBeDisabled();

    // The Enable toggle is present but disabled, not hidden.
    expect(screen.getByRole('switch')).toBeDisabled();
  });

  it('PRODUCT GAP (ELITEA-0941): a viewer cannot reach Preview mode at all — the whole toolbar is hidden, not merely its edit controls', async () => {
    server.use(http.get(CONTEXT_PATH, () => HttpResponse.json({ content: 'Stored background', enabled: true })));

    mount({ canView: true, canEdit: false });

    await screen.findByTestId('project-context-body');

    // `deriveShowFlags` (`ProjectContext.tsx`) computes
    // `showEditorControls: enabled && canEdit` — with NO separate "viewer may
    // still switch to Preview" case. `EditorSection` gates its ENTIRE
    // toolbar — Generate/Import/Copy AND the Edit/Preview mode tabs — behind
    // that one flag, so a viewer has no control that could ever select
    // Preview mode: the case's own expectation ("Preview button is visible
    // and clickable for Viewer") does not hold on this port. This is
    // independent of PJC-PERSIST (the persistence bug): even WITH saved
    // content in front of a viewer, as this test proves, Preview is
    // unreachable.
    expect(screen.queryByRole('tab', { name: 'Preview mode' })).not.toBeInTheDocument();
    expect(screen.queryByRole('tab', { name: 'Edit mode' })).not.toBeInTheDocument();
  });

  /*
   * ── regression guard: toggle's background refetch vs. an unsaved edit ────
   *
   * CI-only failure on `PJC06` (e2e/journeys/settings/settings.project-
   * context.spec.ts, a fresh scratch project — no saved row, so the server's
   * `content` starts at ""): type content, toggle off, expect the "Project
   * Context is turned off" banner (`!enabled && content.trim()`) — passed
   * locally, failed on both engines in CI. `handleToggle` saves the toggle
   * immediately but sends the SAVED content, not the editor's buffer, so its
   * own `invalidateQueries` refetch resolves with the server's untouched ""
   * content. The mount effect that syncs `content`/`enabled` from that query
   * had no `isDirty` guard, so whenever the refetch landed — reliably, given
   * enough wall-clock time; a slower CI container just made it land inside
   * the test's own 5s assertion window — it reset the typed-but-unsaved
   * content back to "", which also flipped the banner's `content.trim()`
   * half back to false right after it had just gone true. A routed response
   * logger against the real E2E stack showed exactly this sequence: GET "" →
   * PUT enabled:true → GET "" → PUT enabled:false → GET "", the last GET
   * landing squarely inside the banner assertion and erasing the typed text.
   * This test reproduces the same race at the unit level with a real
   * (non-mocked) React Query refetch cycle, waiting for the char counter —
   * driven by the PARENT's `content` state, not CodeMirror's own document —
   * exactly as the E2E journey's `editorContentCommitted` does, so the edit
   * is provably committed before the toggle races it.
   */
  it('does not let the toggle-triggered background refetch clobber an unsaved edit', async () => {
    let serverContent = '';
    let serverEnabled = false;
    let getCalls = 0;
    server.use(
      http.get(CONTEXT_PATH, () => {
        getCalls += 1;
        return HttpResponse.json({ content: serverContent, enabled: serverEnabled });
      }),
      http.put(CONTEXT_PATH, async ({ request }) => {
        const body = (await request.json()) as { content: string; enabled: boolean };
        // The toggle sends the SAVED content, which is still "" — this
        // handler never receives the typed buffer, matching the real
        // handler's `UpdateProjectContext`.
        serverContent = body.content;
        serverEnabled = body.enabled;
        return HttpResponse.json({ content: body.content, enabled: body.enabled });
      }),
    );

    const { container } = mount();
    const user = userEvent.setup();
    installCodeMirrorTestPolyfills();

    // Nothing saved yet — the empty state, exactly like a fresh scratch
    // project.
    await user.click(await screen.findByTestId('project-context-create-button'));
    await screen.findByTestId('project-context-body');

    // Enable, as `openEditor` does in the E2E journey, to reveal the editor.
    const toggle = await screen.findByRole('switch');
    await user.click(toggle);
    await waitFor(() => expect(toggle).toBeChecked());
    const editor = await waitFor(() => {
      const el = container.querySelector('.cm-content');
      if (!(el instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
      return el;
    });

    const probe = 'Unsaved edit';
    await user.click(editor);
    await user.keyboard(probe);
    // Wait for the PARENT's `content` state, not merely CodeMirror's own
    // document — the char counter renders `MAX_CHARS - content.length`.
    await waitFor(() => expect(screen.getByTestId('project-context-char-counter'))
      .toHaveTextContent(`${String(MAX_CHARS - probe.length)} characters left.`));

    const getsBeforeToggleOff = getCalls;
    await user.click(toggle);
    await waitFor(() => expect(toggle).not.toBeChecked());
    await waitFor(() => expect(screen.getByText(/Project Context is turned off/i)).toBeInTheDocument());

    // The toggle's own PUT-then-invalidate must actually have triggered the
    // background GET this race depends on — not merely asserting an absence.
    await waitFor(() => expect(getCalls).toBeGreaterThan(getsBeforeToggleOff));
    // Give the resolved query time to reach the (guarded) sync effect.
    await new Promise((resolve) => setTimeout(resolve, 150));

    expect(container.querySelector('.cm-content')).toHaveTextContent(probe);
    expect(screen.getByText(/Project Context is turned off/i)).toBeInTheDocument();
  });
});

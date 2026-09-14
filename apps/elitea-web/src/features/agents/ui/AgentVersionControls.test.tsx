import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { AgentVersionControls } from './AgentVersionControls';

import type { ComponentProps, ReactNode } from 'react';

const versions = [
  { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z' },
  { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z' },
] as const;

function withQueryClient(node: ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>;
}

function renderControls(overrides: Partial<ComponentProps<typeof AgentVersionControls>> = {}) {
  return renderWithTheme(
    withQueryClient(
      <AgentVersionControls
        applicationId="42"
        projectId="9"
        versions={versions}
        activeVersionId={1}
        onSelectVersion={vi.fn()}
        versionBody={{ instructions: 'do the thing' }}
        canSaveNewVersion
        onNewVersionSaved={vi.fn()}
        {...overrides}
      />,
    ),
  );
}

describe('AgentVersionControls', () => {
  beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
  afterEach(() => resetGeneratedClient());

  it('saves the selected skill source, not the first or default version', async () => {
    let body: unknown;
    server.use(
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ id: '3', application_id: '42', name: 'copied', status: 'draft' }, { status: 201 });
      }),
    );
    const { getByRole } = renderControls({ activeVersionId: 2 });
    await userEvent.click(getByRole('button', { name: /save as version/i }));
    await userEvent.type(getByRole('textbox'), 'copied');
    await userEvent.click(getByRole('button', { name: /^save$/i }));
    await waitFor(() =>
      expect(body).toEqual({ name: 'copied', instructions: 'do the thing', copy_skills_from_version_id: 2 }),
    );
  });

  it('renders the version selector trigger — the affordance issue 134 found missing from the agent edit page', () => {
    const { getByTestId } = renderControls();
    expect(getByTestId('version-selector-trigger')).toBeInTheDocument();
  });

  it('lists every version in the menu and reports the picked one to the caller', async () => {
    const onSelectVersion = vi.fn();
    const { getByTestId, getAllByRole, getByRole } = renderControls({ onSelectVersion });

    await userEvent.click(getByTestId('version-selector-trigger'));
    // Scoped to menu items: 'base' also labels the closed trigger, so an
    // unscoped text query cannot tell a populated menu from an empty one.
    // #147 added one COMMAND item ("Set as default") to the same menu; it is
    // excluded by test id so this stays an assertion about the version rows.
    const versionRows = getAllByRole('menuitem').filter(
      (item) => item.dataset['testid'] !== 'agent-version-set-default',
    );
    expect(versionRows.map((item) => item.textContent)).toEqual([
      expect.stringContaining('base'),
      expect.stringContaining('v1'),
    ]);
    await userEvent.click(getByRole('menuitem', { name: /^v1/ }));

    expect(onSelectVersion).toHaveBeenCalledTimes(1);
    expect(onSelectVersion.mock.calls[0]?.[0]).toMatchObject({ id: 2, name: 'v1' });
  });

  it('renders "Save As Version" when the viewer may write', () => {
    const { getByRole } = renderControls();
    expect(getByRole('button', { name: /save as version/i })).toBeInTheDocument();
  });

  it('hides "Save As Version" for a read-only viewer but keeps the selector', () => {
    const { queryByRole, getByTestId } = renderControls({ canSaveNewVersion: false });
    expect(queryByRole('button', { name: /save as version/i })).not.toBeInTheDocument();
    expect(getByTestId('version-selector-trigger')).toBeInTheDocument();
  });

  it('rejects a duplicate version name before sending anything, using the names it was given', async () => {
    const { getByRole, findByText } = renderControls();

    await userEvent.click(getByRole('button', { name: /save as version/i }));
    await userEvent.type(getByRole('textbox'), 'v1');
    await userEvent.click(getByRole('button', { name: /^save$/i }));

    expect(await findByText(/already exists/i)).toBeInTheDocument();
  });
});

const SET_DEFAULT_ROUTE = '*/elitea_core/default_version/prompt_lib/:projectId/:applicationId/:versionId';

/**
 * #147 — JRNY-015's middle step. The PATCH route, the Go handler, the repo
 * write and the generated `setApplicationDefaultVersion` all existed; the
 * menu that should reach them had version rows and nothing else. These tests
 * are about the COMPOSITION and the REQUEST, because "the hook works" was
 * already true and meant nothing — the same failure mode #134/#307 record.
 */
describe('AgentVersionControls — set default version', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('offers a set-default item inside the version menu the page actually mounts', async () => {
    const { getByTestId } = renderControls({ activeVersionId: 2 });

    await userEvent.click(getByTestId('version-selector-trigger'));

    const item = getByTestId('agent-version-set-default');
    expect(item).toBeInTheDocument();
    // Enabled, not merely present: a rendered-but-permanently-disabled item
    // would satisfy a presence-only assertion while reaching nothing.
    expect(item).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('PATCHes only after the confirm dialog is confirmed, then marks that version as the default', async () => {
    const user = userEvent.setup();
    const requests: string[] = [];
    server.use(
      http.patch(SET_DEFAULT_ROUTE, ({ request }) => {
        requests.push(new URL(request.url).pathname);
        return HttpResponse.json({ ok: true }, { status: 200 });
      }),
    );
    const { getByTestId, getByRole, queryByTestId } = renderControls({ activeVersionId: 2 });

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-set-default'));

    // Opening the dialog must not, by itself, change the default.
    expect(getByTestId('agent-version-set-default-name')).toHaveTextContent('v1');
    expect(requests).toHaveLength(0);

    await user.click(getByRole('button', { name: /set as a default/i }));

    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]).toContain('/9/42/2');

    // The options this render was given carry no `is_default`, so the only
    // thing that can mark a default here is the write that just succeeded.
    await waitFor(() => expect(queryByTestId('agent-version-set-default-name')).not.toBeInTheDocument());
    await user.click(getByTestId('version-selector-trigger'));
    expect(getByTestId('agent-version-default-marker')).toBeInTheDocument();
    expect(getByTestId('agent-version-set-default')).toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps the dialog open and shows the refusal when the server rejects the version', async () => {
    const user = userEvent.setup();
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ error: 'version not found' }, { status: 404 })));
    const { getByTestId, getByRole, queryByTestId } = renderControls({ activeVersionId: 2 });

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-set-default'));
    await user.click(getByRole('button', { name: /set as a default/i }));

    await waitFor(() => expect(getByRole('alert')).toBeInTheDocument());
    expect(getByTestId('agent-version-set-default-name')).toBeInTheDocument();

    // …and nothing claims the default moved.
    await user.click(getByRole('button', { name: /^cancel$/i }));
    await user.click(getByTestId('version-selector-trigger'));
    expect(queryByTestId('agent-version-default-marker')).not.toBeInTheDocument();
  });

  // The read half of #147. `versions[].is_default` comes from the Go handler's
  // `getVersions`, derived from `applications.meta.default_version_id`. Before
  // it existed the component could only know a default it had set ITSELF, so a
  // reload showed no default at all — and the "Set as default" item stayed
  // enabled on the version that already was one.
  it('marks the default the SERVER reports, with no interaction and no prior write', async () => {
    const { getByTestId, getAllByRole } = renderControls({
      activeVersionId: 1,
      versions: [
        { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z', is_default: false },
        { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z', is_default: true },
      ],
    });

    await userEvent.click(getByTestId('version-selector-trigger'));

    expect(getByTestId('agent-version-default-marker')).toBeInTheDocument();
    // On the RIGHT row: the marker is rendered inside its version's menu item,
    // so a component that marked the first row would still pass a bare
    // "is it in the document" assertion.
    const markedRow = getAllByRole('menuitem').find((item) =>
      item.querySelector('[data-testid="agent-version-default-marker"]') !== null,
    );
    expect(markedRow?.textContent).toContain('v1');
  });

  // A list with no flagged version means "no default recorded", which is a
  // different answer from "this list cannot say" — both render no marker, and
  // neither may invent one.
  it('marks nothing when no version carries the flag', async () => {
    const { getByTestId, queryByTestId } = renderControls({
      versions: [
        { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z', is_default: false },
        { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z' },
      ],
    });

    await userEvent.click(getByTestId('version-selector-trigger'));
    expect(queryByTestId('agent-version-default-marker')).not.toBeInTheDocument();
  });

  // The remembered id is an OVERRIDE, not a second source of truth: in the
  // window between a successful PATCH and the next detail fetch, the flag this
  // render was given is stale by exactly the write that just happened.
  it('prefers the default it just set over the stale flag it was rendered with', async () => {
    const user = userEvent.setup();
    server.use(http.patch(SET_DEFAULT_ROUTE, () => HttpResponse.json({ ok: true }, { status: 200 })));
    const { getByTestId, getByRole, getAllByRole, queryByTestId } = renderControls({
      activeVersionId: 1,
      versions: [
        { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z', is_default: false },
        { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z', is_default: true },
      ],
    });

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-set-default'));
    await user.click(getByRole('button', { name: /set as a default/i }));
    // The dialog closing is the signal the write succeeded — it is left open
    // on refusal, which is what the sibling refusal test pins.
    await waitFor(() => expect(queryByTestId('agent-version-set-default-name')).not.toBeInTheDocument());

    await user.click(getByTestId('version-selector-trigger'));
    const markedRow = getAllByRole('menuitem').find((item) =>
      item.querySelector('[data-testid="agent-version-default-marker"]') !== null,
    );
    expect(markedRow?.textContent).toContain('base');
  });

  it('offers no set-default item to a read-only viewer, and none before the project id resolves', async () => {
    const readOnly = renderControls({ activeVersionId: 2, canSaveNewVersion: false });
    await userEvent.click(readOnly.getByTestId('version-selector-trigger'));
    expect(readOnly.queryByTestId('agent-version-set-default')).not.toBeInTheDocument();
    readOnly.unmount();

    const noProject = renderControls({ activeVersionId: 2, projectId: undefined });
    await userEvent.click(noProject.getByTestId('version-selector-trigger'));
    expect(noProject.queryByTestId('agent-version-set-default')).not.toBeInTheDocument();
  });
});

const DELETE_ROUTE = '*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId';

/**
 * #147, the other half. The DELETE route, the Go handler and `useDeleteVersion`
 * all existed. The affordance was an icon button beside the menu, the version
 * it acted on was always the page's ACTIVE one, and the server's refusal
 * reached nobody: the icon button reported it through an `onError` callback
 * the agent page never supplied, and `DeleteEntityModal` had no place to show
 * one.
 *
 * These tests drive the menu item, the typed-name confirm and the request.
 */
describe('AgentVersionControls — delete version', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  function renderDeletable(overrides: Partial<ComponentProps<typeof AgentVersionControls>> = {}) {
    return renderControls({ activeVersionId: 2, versionDelete: { onVersionDeleted: vi.fn() }, ...overrides });
  }

  it('offers a delete item inside the version menu, enabled for an ordinary version', async () => {
    const { getByTestId } = renderDeletable();

    await userEvent.click(getByTestId('version-selector-trigger'));

    const item = getByTestId('agent-version-delete');
    expect(item).toBeInTheDocument();
    // Enabled, not merely present: the whole class of defect this issue
    // belongs to is a control that renders and reaches nothing.
    expect(item).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('refuses the "base" version and the version that is the default', async () => {
    const onBase = renderDeletable({ activeVersionId: 1 });
    await userEvent.click(onBase.getByTestId('version-selector-trigger'));
    expect(onBase.getByTestId('agent-version-delete')).toHaveAttribute('aria-disabled', 'true');
    onBase.unmount();

    const onDefault = renderDeletable({
      versions: [
        { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z', is_default: false },
        { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z', is_default: true },
      ],
    });
    await userEvent.click(onDefault.getByTestId('version-selector-trigger'));
    expect(onDefault.getByTestId('agent-version-delete')).toHaveAttribute('aria-disabled', 'true');
  });

  it('sends nothing until the version name is typed, then DELETEs that version and drops it from the menu', async () => {
    const user = userEvent.setup();
    const onVersionDeleted = vi.fn();
    const requests: string[] = [];
    server.use(
      http.delete(DELETE_ROUTE, ({ request }) => {
        requests.push(new URL(request.url).pathname);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { getByTestId, getByRole, getAllByRole } = renderDeletable({ versionDelete: { onVersionDeleted } });

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-delete'));

    // The confirm button is disabled until the typed name matches. This is
    // the safeguard, not decoration: an empty field must reach nothing.
    expect(getByRole('button', { name: /^delete$/i })).toBeDisabled();
    expect(requests).toHaveLength(0);

    // A WRONG name is still refused. An assertion on the empty field alone
    // would pass for a dialog that enabled Confirm on any keystroke.
    await user.type(getByRole('textbox'), 'v');
    expect(getByRole('button', { name: /^delete$/i })).toBeDisabled();
    await user.clear(getByRole('textbox'));

    await user.type(getByRole('textbox'), 'v1');
    await user.click(getByRole('button', { name: /^delete$/i }));

    await waitFor(() => expect(requests).toHaveLength(1));
    // The version the MENU offered, which is the whole point of moving the
    // trigger there: /{projectId}/{applicationId}/{versionId}.
    expect(requests[0]).toContain('/9/42/2');
    await waitFor(() => expect(onVersionDeleted).toHaveBeenCalledTimes(1));

    // The optimistic update. The `versions` prop still holds the deleted row
    // (the page's detail query has not been refetched yet), so a menu that
    // simply re-rendered the prop would still offer it.
    await user.click(getByTestId('version-selector-trigger'));
    const rows = getAllByRole('menuitem').filter((item) => item.dataset['testid'] === undefined);
    expect(rows.map((item) => item.textContent)).toEqual([expect.stringContaining('base')]);
  });

  it('keeps the dialog open and shows the SERVER’s own refusal, not a diagnostic', async () => {
    const user = userEvent.setup();
    const onVersionDeleted = vi.fn();
    const onVersionDeleteError = vi.fn();
    server.use(
      http.delete(DELETE_ROUTE, () =>
        HttpResponse.json({ error: 'Unpublish first. Cannot delete a published version.' }, { status: 400 }),
      ),
    );
    const { getByTestId, getByRole } = renderDeletable({ versionDelete: { onVersionDeleted, onVersionDeleteError } });

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-delete'));
    await user.type(getByRole('textbox'), 'v1');
    await user.click(getByRole('button', { name: /^delete$/i }));

    // The exact server text. `EliteaApiError.message` is
    // `eliteaFetch: 400 from <url>`, which is what a user used to be told when
    // anything at all was reported.
    await waitFor(() =>
      expect(getByRole('alert')).toHaveTextContent('Unpublish first. Cannot delete a published version.'),
    );
    // Still open: closing it would read as "deleted".
    expect(getByTestId('agent-version-delete-dialog')).toBeInTheDocument();
    expect(onVersionDeleted).not.toHaveBeenCalled();
    expect(onVersionDeleteError).toHaveBeenCalledWith('Unpublish first. Cannot delete a published version.');
  });

  it('offers no delete item to a read-only viewer, without a caller, or before the project id resolves', async () => {
    const readOnly = renderDeletable({ canSaveNewVersion: false });
    await userEvent.click(readOnly.getByTestId('version-selector-trigger'));
    expect(readOnly.queryByTestId('agent-version-delete')).not.toBeInTheDocument();
    readOnly.unmount();

    const noCaller = renderControls({ activeVersionId: 2 });
    await userEvent.click(noCaller.getByTestId('version-selector-trigger'));
    expect(noCaller.queryByTestId('agent-version-delete')).not.toBeInTheDocument();
    noCaller.unmount();

    const noProject = renderDeletable({ projectId: undefined });
    await userEvent.click(noProject.getByTestId('version-selector-trigger'));
    expect(noProject.queryByTestId('agent-version-delete')).not.toBeInTheDocument();
  });
});

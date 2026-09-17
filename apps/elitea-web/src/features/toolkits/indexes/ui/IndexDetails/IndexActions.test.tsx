import { ThemeProvider } from '@mui/material/styles';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

/** `DiscardButton`'s confirm modal (`BaseModal`) reads `theme.vars.palette.*` — this file drives its own `RouterProvider` (needed for `IndexActions.tsx`'s own `useSelectedProjectId`), so the theme has to be wired in here too, matching `features/agents/ui/DeleteApplicationButton.test.tsx`'s identical, already-established pattern. */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

import { useIndexesStore } from '../../model/indexesStore';

import { IndexActions } from './IndexActions';
import type { IndexActionsProps, UseToolkitSchemasResult } from './IndexActions';

const BASE = '/api/v2';

const noSchemas: UseToolkitSchemasResult = { toolkitSchemas: {}, isFetching: false };

const baseProps: Omit<IndexActionsProps, 'useToolkitSchemas'> = {
  activeView: 'run',
  index: { id: '1', metadata: { collection: 'my-index', state: 'completed' } },
  view: 'create',
  toolkitId: 'tk-1',
  onDiscard: vi.fn(),
  indexData: vi.fn(),
  handleDeleteIndex: vi.fn(),
  selectedIndexTools: ['index_data', 'remove_index'],
  onCancelIndexing: vi.fn(),
};

function renderActions(overrides: Partial<IndexActionsProps> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <IndexActions
            {...baseProps}
            useToolkitSchemas={() => noSchemas}
            {...overrides}
          />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('IndexActions — create view', () => {
  it('renders Cancel + Index, Index disabled when the form is invalid', async () => {
    renderActions({ view: 'create', isValidForm: false });
    expect(await screen.findByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Index' })).toBeDisabled();
  });

  it('Index is enabled and calls indexData when the form is valid', async () => {
    const user = userEvent.setup();
    const indexData = vi.fn();
    renderActions({ view: 'create', isValidForm: true, indexData });
    const button = await screen.findByRole('button', { name: 'Index' });
    expect(button).toBeEnabled();
    await user.click(button);
    expect(indexData).toHaveBeenCalled();
  });

  it('Cancel opens a confirm dialog, and confirming calls onDiscard', async () => {
    const user = userEvent.setup();
    const onDiscard = vi.fn();
    renderActions({ view: 'create', onDiscard });
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    await user.click(await screen.findByRole('button', { name: 'Discard' }));
    expect(onDiscard).toHaveBeenCalled();
  });
});

describe('IndexActions — edit view', () => {
  it('renders Reindex + Delete + Schedule switch', async () => {
    renderActions({ view: 'edit', activeView: 'configuration' });
    expect(await screen.findByRole('button', { name: 'Reindex' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(screen.getByText('Schedule')).toBeInTheDocument();
  });

  it('Reindex is disabled while on the "run" tab (must switch to Configuration first)', async () => {
    renderActions({ view: 'edit', activeView: 'run' });
    expect(await screen.findByRole('button', { name: 'Reindex' })).toBeDisabled();
  });

  /* elitea_issues: #5984 — reindexing must be confirmed before it starts ("Reindex confirmation
   * / Are you sure to reindex the [index name] index? ... Cancel | Reindex"). */
  it('Reindex opens a confirm dialog naming the index, and confirming calls indexData', async () => {
    const user = userEvent.setup();
    const indexData = vi.fn();
    renderActions({ view: 'edit', activeView: 'configuration', indexData });
    const button = await screen.findByRole('button', { name: 'Reindex' });
    await user.click(button);
    expect(indexData).not.toHaveBeenCalled();
    const dialog = await within(document.body).findByRole('dialog');
    expect(within(dialog).getByText('Reindex confirmation')).toBeInTheDocument();
    expect(within(dialog).getByText(/reindex the my-index index/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Reindex' }));
    expect(indexData).toHaveBeenCalled();
  });

  it('Reindex confirm dialog: Cancel dismisses without calling indexData', async () => {
    const user = userEvent.setup();
    const indexData = vi.fn();
    renderActions({ view: 'edit', activeView: 'configuration', indexData });
    await user.click(await screen.findByRole('button', { name: 'Reindex' }));
    const dialog = await within(document.body).findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(indexData).not.toHaveBeenCalled();
  });

  it('Delete is disabled when the "remove_index" tool is not selected', async () => {
    renderActions({ view: 'edit', activeView: 'configuration', selectedIndexTools: ['index_data'] });
    expect(await screen.findByRole('button', { name: 'Delete' })).toBeDisabled();
  });

  it('Delete calls handleDeleteIndex when enabled', async () => {
    const user = userEvent.setup();
    const handleDeleteIndex = vi.fn();
    renderActions({ view: 'edit', activeView: 'configuration', selectedIndexTools: ['remove_index'], handleDeleteIndex });
    await user.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(handleDeleteIndex).toHaveBeenCalled();
  });

  it('schedule switch is disabled with an insufficient-permissions tooltip when the user lacks the permission', async () => {
    renderActions({ view: 'edit', activeView: 'configuration', userPermissions: [], currentProjectName: 'Acme' });
    await screen.findByText('Schedule');
    const switchInput = screen.getByRole('switch');
    expect(switchInput).toBeDisabled();
  });

  it('falls back to the hardcoded schedule default, not the raw scheduler bucket, when neither a per-user nor a -1 schedule exists', async () => {
    // Baseline (`IndexActions.jsx:71-75`): `schedules?.[userId] ??
    // schedules?.[-1] ?? {cron: default, enabled: false, credentials: null}`
    // — only two real candidates before the hardcoded default. Seed a
    // `toolkitScheduler` bucket whose `schedules` map has neither key, with
    // a stray top-level `enabled: true` on the bucket itself (the shape the
    // pre-fix `?? entryByUser` fallback would incorrectly read `.enabled`
    // off, instead of falling through to the hardcoded `false` default).
    useIndexesStore.setState({
      toolkitScheduler: { 'my-index': { enabled: true, schedules: { 7: { enabled: true, cron: '0 0 * * 1' } } } },
    });
    renderActions({ view: 'edit', activeView: 'configuration' });
    const switchInput = await screen.findByRole('switch');
    expect(switchInput).not.toBeChecked();
  });

  it('toggling the schedule switch PATCHes updateIndexSchedule with the flipped enabled flag', async () => {
    const user = userEvent.setup();
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/index_meta/prompt_lib/proj-1/tk-1/my-index`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({});
      }),
    );
    renderActions({ view: 'edit', activeView: 'configuration', userPermissions: ['models.applications.index_meta.edit'] });
    const switchInput = await screen.findByRole('switch');
    await user.click(switchInput);
    await waitFor(() => expect(capturedBody).toMatchObject({ enabled: true }));
  });

  /* elitea_issues: #6547 — updating a schedule must send the CURRENT user's time zone, not the
   * original creation-time zone the stored schedule record carries. `handleChangeIndexSchedule`
   * builds its request body as `{..., timezone, ...data}` before the fix, and `data` is
   * `{...scheduleData, ...}` — a stale `scheduleData.timezone` from a previous save would win
   * over the freshly resolved one. Seed a stored schedule with an implausible stale timezone and
   * assert the PATCH never carries it. */
  it('always sends the current timezone on update, never a stale one carried by the stored schedule', async () => {
    const user = userEvent.setup();
    let capturedBody: { timezone?: unknown } | undefined;
    server.use(
      http.patch(`${BASE}/elitea_core/index_meta/prompt_lib/proj-1/tk-1/my-index`, async ({ request }) => {
        capturedBody = (await request.json()) as { timezone?: unknown };
        return HttpResponse.json({});
      }),
    );
    useIndexesStore.setState({
      toolkitScheduler: {
        'my-index': { schedules: { '-1': { enabled: false, cron: '0 0 * * 1', credentials: null, timezone: 'Pacific/Kiritimati' } } },
      },
    });
    renderActions({ view: 'edit', activeView: 'configuration', userPermissions: ['models.applications.index_meta.edit'] });
    const switchInput = await screen.findByRole('switch');
    await user.click(switchInput);
    await waitFor(() => expect(capturedBody).toBeDefined());
    expect(capturedBody?.timezone).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
    expect(capturedBody?.timezone).not.toBe('Pacific/Kiritimati');
  });
});

describe('IndexActions — indexing in progress', () => {
  it('shows a Delete-only removeButton when indexing cannot be stopped and there is no task_id', async () => {
    renderActions({ isIndexingData: true, index: { id: '1', metadata: { collection: 'my-index' } } });
    expect(await screen.findByRole('button', { name: 'Delete' })).toBeInTheDocument();
  });

  it('shows a Stop discard button when a task_id is present', async () => {
    const onCancelIndexing = vi.fn();
    renderActions({
      isIndexingData: true,
      index: { id: '1', metadata: { collection: 'my-index', task_id: 'task-9' } },
      onCancelIndexing,
    });
    expect(await screen.findByRole('button', { name: 'Stop' })).toBeInTheDocument();
  });
});

/**
 * ELITEA-2880 / 2883 / 2887 — the Save / Save & Reindex split.
 *
 * The rule the cases state four different ways: a CLEAN configuration form
 * offers "Reindex" and nothing else; a DIRTY one offers "Save" and
 * "Save & Reindex" and withdraws "Reindex". These assert both directions,
 * because a change that rendered all three at once would satisfy either half
 * alone.
 */
describe('IndexActions — Save / Save & Reindex (ELITEA-2880, ELITEA-2883)', () => {
  const configSave = { isDirty: false, isSaving: false, onSave: vi.fn(), onSaveAndReindex: vi.fn() };

  it('offers only Reindex while the form is clean', async () => {
    renderActions({ view: 'edit', activeView: 'configuration', configSave: { ...configSave, isDirty: false } });
    expect(await screen.findByRole('button', { name: 'Reindex' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save & Reindex' })).not.toBeInTheDocument();
  });

  it('swaps Reindex for Save + Save & Reindex the moment the form is dirty', async () => {
    renderActions({ view: 'edit', activeView: 'configuration', configSave: { ...configSave, isDirty: true } });
    expect(await screen.findByRole('button', { name: 'Save' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Save & Reindex' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Reindex' })).not.toBeInTheDocument();
  });

  it('Save calls onSave and does NOT start a reindex', async () => {
    const user = userEvent.setup();
    const onSave = vi.fn();
    const onSaveAndReindex = vi.fn();
    const indexData = vi.fn();
    renderActions({
      view: 'edit',
      activeView: 'configuration',
      indexData,
      configSave: { isDirty: true, isSaving: false, onSave, onSaveAndReindex },
    });
    await user.click(await screen.findByRole('button', { name: 'Save' }));
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onSaveAndReindex).not.toHaveBeenCalled();
    expect(indexData).not.toHaveBeenCalled();
  });

  it('Save & Reindex calls onSaveAndReindex, never `indexData` directly — the reindex has to follow the save', async () => {
    const user = userEvent.setup();
    const onSaveAndReindex = vi.fn();
    const indexData = vi.fn();
    renderActions({
      view: 'edit',
      activeView: 'configuration',
      indexData,
      configSave: { isDirty: true, isSaving: false, onSave: vi.fn(), onSaveAndReindex },
    });
    await user.click(await screen.findByRole('button', { name: 'Save & Reindex' }));
    expect(onSaveAndReindex).toHaveBeenCalledTimes(1);
    expect(indexData).not.toHaveBeenCalled();
  });

  it('disables both buttons while the save is in flight', async () => {
    renderActions({ view: 'edit', activeView: 'configuration', configSave: { ...configSave, isDirty: true, isSaving: true } });
    expect(await screen.findByRole('button', { name: 'Save' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Save & Reindex' })).toBeDisabled();
  });

  it('keeps the pre-split behaviour for a caller that wires no save at all', async () => {
    renderActions({ view: 'edit', activeView: 'configuration' });
    expect(await screen.findByRole('button', { name: 'Reindex' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });
});

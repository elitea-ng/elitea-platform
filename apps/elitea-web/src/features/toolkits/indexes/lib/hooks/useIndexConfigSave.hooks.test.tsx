/**
 * The Save half of the Indexes tab's Save / Save & Reindex split
 * (ELITEA-2880 … ELITEA-2887).
 *
 * The hook needs TanStack Router context (`useSelectedProjectId`) and a real
 * `QueryClient` (the PUT mutation), so this drives a real `RouterProvider`
 * with a probe component — the same shape, and for the same reason, as
 * `useIndexHistory.hooks.test.tsx` beside it.
 *
 * Every request below goes through MSW at the REAL path, so a change to the
 * URL the client builds fails here rather than at run time.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useIndexesStore } from '../../model/indexesStore';

import { INDEX_CONFIG_SAVE_MESSAGES, useIndexConfigSave } from './useIndexConfigSave.hooks';
import type { IndexConfigSaveParams } from './useIndexConfigSave.hooks';

const BASE = '/api/v2';
const SAVE_PATH = `${BASE}/elitea_core/index_meta/prompt_lib/proj-1/tk-1/my-index/configuration`;

const SCHEMA_KEYS = ['index_name', 'clean_index', 'progress_step'];
const STORED = { index_name: 'my-index', clean_index: false, progress_step: 50 };

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

afterEach(() => {
  resetGeneratedClient();
});

function Probe(props: { readonly params: Partial<IndexConfigSaveParams> }): React.ReactNode {
  const result = useIndexConfigSave({
    projectId: 'proj-1',
    toolkitId: 'tk-1',
    indexName: 'my-index',
    indexId: 'idx-1',
    schemaKeys: SCHEMA_KEYS,
    storedConfiguration: STORED,
    current: STORED,
    isConfigurationTab: true,
    isValidForm: true,
    onReindex: () => {},
    ...props.params,
  });
  return (
    <div>
      <span data-testid="dirty">{String(result.isDirty)}</span>
      <span data-testid="saving">{String(result.isSaving)}</span>
      <button
        type="button"
        onClick={result.onSave}
      >
        save
      </button>
      <button
        type="button"
        onClick={result.onSaveAndReindex}
      >
        save-reindex
      </button>
    </div>
  );
}

function renderHookProbe(params: Partial<IndexConfigSaveParams> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <Probe params={params} />
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

describe('useIndexConfigSave — dirty reporting', () => {
  it('is clean when the form still holds the stored configuration', async () => {
    renderHookProbe();
    expect(await screen.findByTestId('dirty')).toHaveTextContent('false');
  });

  it('is dirty when a value differs', async () => {
    renderHookProbe({ current: { ...STORED, progress_step: 75 } });
    expect(await screen.findByTestId('dirty')).toHaveTextContent('true');
  });

  it('is never dirty outside the configuration tab — the run and history tabs have no form to save', async () => {
    renderHookProbe({ current: { ...STORED, progress_step: 75 }, isConfigurationTab: false });
    expect(await screen.findByTestId('dirty')).toHaveTextContent('false');
  });

  it('reports the dirty flag upward for the page-level unsaved-changes guard (ELITEA-2885)', async () => {
    const onDirtyChange = vi.fn();
    const view = renderHookProbe({ current: { ...STORED, progress_step: 75 }, onDirtyChange });
    await waitFor(() => expect(onDirtyChange).toHaveBeenCalledWith(true));
    // Unmount must lower it: the guard's store is a module singleton, and a
    // closed panel may not leave the whole app blocked.
    onDirtyChange.mockClear();
    view.unmount();
    expect(onDirtyChange).toHaveBeenCalledWith(false);
  });
});

describe('useIndexConfigSave — Save (ELITEA-2880, ELITEA-2886)', () => {
  it('PUTs only the schema keys, reports success, and does NOT reindex', async () => {
    const user = userEvent.setup();
    let body: unknown;
    server.use(
      http.put(SAVE_PATH, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    const onSuccess = vi.fn();
    const onReindex = vi.fn();
    renderHookProbe({
      current: { ...STORED, progress_step: 75, stray_key: 'not in the schema' },
      onSuccess,
      onReindex,
    });
    await user.click(await screen.findByRole('button', { name: 'save' }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalledWith(INDEX_CONFIG_SAVE_MESSAGES.saved));
    expect(body).toEqual({ index_configuration: { index_name: 'my-index', clean_index: false, progress_step: 75 } });
    expect(onReindex).not.toHaveBeenCalled();
  });

  it('goes clean in the same commit as the save — without waiting for the list query to come back', async () => {
    const user = userEvent.setup();
    server.use(http.put(SAVE_PATH, () => HttpResponse.json({ ok: true })));
    renderHookProbe({ current: { ...STORED, progress_step: 75 } });
    expect(await screen.findByTestId('dirty')).toHaveTextContent('true');
    await user.click(screen.getByRole('button', { name: 'save' }));
    await waitFor(() => expect(screen.getByTestId('dirty')).toHaveTextContent('false'));
  });

  it('reports a refused save and leaves the form dirty so the user can retry (ELITEA-2886 step 14)', async () => {
    const user = userEvent.setup();
    server.use(http.put(SAVE_PATH, () => HttpResponse.json({ ok: false, error: 'nope' }, { status: 400 })));
    const onError = vi.fn();
    const onSuccess = vi.fn();
    renderHookProbe({ current: { ...STORED, progress_step: 75 }, onError, onSuccess });
    await user.click(await screen.findByRole('button', { name: 'save' }));
    await waitFor(() => expect(onError).toHaveBeenCalledWith(INDEX_CONFIG_SAVE_MESSAGES.failed));
    expect(onSuccess).not.toHaveBeenCalled();
    expect(screen.getByTestId('dirty')).toHaveTextContent('true');
  });
});

describe('useIndexConfigSave — Save & Reindex (ELITEA-2881, ELITEA-2882)', () => {
  it('saves FIRST and only then starts the reindex', async () => {
    const user = userEvent.setup();
    const order: string[] = [];
    server.use(
      http.put(SAVE_PATH, () => {
        order.push('save');
        return HttpResponse.json({ ok: true });
      }),
    );
    const onReindex = vi.fn(() => void order.push('reindex'));
    const onSuccess = vi.fn();
    renderHookProbe({ current: { ...STORED, progress_step: 75 }, onReindex, onSuccess });
    await user.click(await screen.findByRole('button', { name: 'save-reindex' }));
    await waitFor(() => expect(onReindex).toHaveBeenCalled());
    expect(order).toEqual(['save', 'reindex']);
    expect(onSuccess).toHaveBeenCalledWith(INDEX_CONFIG_SAVE_MESSAGES.savedAndReindexing);
  });

  /*
   * The row's OVERLAY, not just the form. A reindex of an existing index runs
   * `index.metadata.index_configuration` — the server's copy — rather than the
   * form, which is the rule that makes "Reindex uses the last SAVED
   * configuration" true. Without this patch "Save & Reindex" would store the
   * new configuration and immediately reindex with the old one.
   */
  it('patches the index row with what was just saved, so the reindex that follows reads it', async () => {
    const user = userEvent.setup();
    server.use(http.put(SAVE_PATH, () => HttpResponse.json({ ok: true })));
    renderHookProbe({ current: { ...STORED, progress_step: 75 } });
    await user.click(await screen.findByRole('button', { name: 'save-reindex' }));
    await waitFor(() =>
      expect(useIndexesStore.getState().indexPatches['idx-1']?.index_configuration).toMatchObject({ progress_step: 75 }),
    );
  });

  it('does not reindex when the save is refused', async () => {
    const user = userEvent.setup();
    server.use(http.put(SAVE_PATH, () => HttpResponse.json({ ok: false }, { status: 500 })));
    const onReindex = vi.fn();
    const onError = vi.fn();
    renderHookProbe({ current: { ...STORED, progress_step: 75 }, onReindex, onError });
    await user.click(await screen.findByRole('button', { name: 'save-reindex' }));
    await waitFor(() => expect(onError).toHaveBeenCalledWith(INDEX_CONFIG_SAVE_MESSAGES.failed));
    expect(onReindex).not.toHaveBeenCalled();
  });

  it('refuses BOTH halves when the form is invalid, and says why (ELITEA-2882)', async () => {
    const user = userEvent.setup();
    let requests = 0;
    server.use(
      http.put(SAVE_PATH, () => {
        requests += 1;
        return HttpResponse.json({ ok: true });
      }),
    );
    const onError = vi.fn();
    const onReindex = vi.fn();
    renderHookProbe({ current: { ...STORED, progress_step: 75 }, isValidForm: false, onError, onReindex });
    await user.click(await screen.findByRole('button', { name: 'save-reindex' }));
    await waitFor(() => expect(onError).toHaveBeenCalledWith(INDEX_CONFIG_SAVE_MESSAGES.invalid));
    await user.click(screen.getByRole('button', { name: 'save' }));
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(2));
    expect(requests).toBe(0);
    expect(onReindex).not.toHaveBeenCalled();
    // The edits survive the refusal.
    expect(screen.getByTestId('dirty')).toHaveTextContent('true');
  });
});

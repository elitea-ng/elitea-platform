/**
 * The index chip and its settings dialog, wired to the in-memory host fake:
 * every chip state, and each dialog flow (turn on, rebuild, cancel, turn
 * off, remove with its confirmation, the policy turning the index off).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { OFF_STATUS, type IndexStatus } from '@/shared/desktop/indexIpc';
import { createFakeIndexIpc, type FakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IndexIpcProvider } from '../model/ipcContext';
import { WorkspaceIndexControl } from './WorkspaceIndexControl';

const READY: Partial<IndexStatus> = { state: 'ready', files: 120, entities: 12_400, relations: 3_000, last_run: '2026-10-10T08:00:00Z' };

function mount(ipc: FakeIndexIpc): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <IndexIpcProvider ipc={ipc}>
        <WorkspaceIndexControl workspaceId="w1" name="app" />
      </IndexIpcProvider>
    </QueryClientProvider>,
  );
}

const chip = (): HTMLElement => screen.getByTestId('index-status-chip');

async function openDialog(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
  await user.click(chip());
  return screen.findByRole('dialog', { name: 'Code index · app' });
}

describe('IndexStatusChip states', () => {
  it.each([
    [{ state: 'off' }, 'Off'],
    [READY, /Ready · 12(\.4)?K entities/],
    [{ ...READY, state: 'stale', changed_files: 3 }, 'Stale'],
    [{ state: 'error', error: 'disk full' }, 'Error'],
    [{ state: 'building' }, 'Building…'],
  ] satisfies [Partial<IndexStatus>, string | RegExp][])('shows %o as %s', async (status, label) => {
    mount(createFakeIndexIpc({ statuses: { w1: status } }));
    await waitFor(() => expect(chip()).toHaveTextContent(label));
  });

  it('counts the files of a running build from the progress events', async () => {
    const ipc = createFakeIndexIpc({ statuses: { w1: { state: 'building' } } });
    mount(ipc);
    await waitFor(() => expect(chip()).toHaveTextContent('Building…'));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    const building = { ...OFF_STATUS, state: 'building' as const };
    act(() => {
      ipc.emit({ workspace_id: 'w1', phase: 'parsing', message: '[extract] Parsing 40 files', status: building });
      ipc.emit({ workspace_id: 'w1', phase: 'building', message: '[progress] 📄 Processed 10 files | 📊 80 entities', status: building });
      // Another folder's progress is not this one's.
      ipc.emit({ workspace_id: 'w2', phase: 'building', message: '[progress] 📄 Processed 30 files | 📊 80 entities', status: building });
    });
    expect(chip()).toHaveTextContent('Building 10/40');
    act(() => ipc.emit({ workspace_id: 'w1', phase: 'ready', message: null, status: { ...OFF_STATUS, ...READY } }));
    expect(chip()).toHaveTextContent(/Ready · 12(\.4)?K entities/);
  });

  it('shows Off, and why, when the policy turns the index off', async () => {
    const user = userEvent.setup();
    mount(createFakeIndexIpc({ policyAllowed: false }));
    await waitFor(() => expect(chip()).toHaveTextContent('Off'));
    const dialog = await openDialog(user);
    expect(within(dialog).getByTestId('index-policy-off')).toHaveTextContent('turned off by your organisation’s policy');
    expect(within(dialog).queryByRole('button', { name: 'Turn on' })).toBeNull();
    expect(within(dialog).queryByRole('button', { name: 'Remove index…' })).toBeNull();
  });
});

describe('IndexSettingsDialog flows', () => {
  it('turns the index on, then cancels the build', async () => {
    const ipc = createFakeIndexIpc();
    const user = userEvent.setup();
    mount(ipc);
    const dialog = await openDialog(user);
    expect(within(dialog).getByTestId('index-embeddings')).toHaveTextContent('coming soon');
    expect(within(dialog).getByRole('switch')).toBeDisabled();

    await user.click(within(dialog).getByRole('button', { name: 'Turn on' }));
    expect(ipc.calls.enabled).toEqual(['w1']);
    await waitFor(() => expect(chip()).toHaveTextContent('Building…'));

    await user.click(within(dialog).getByRole('button', { name: 'Cancel build' }));
    expect(ipc.calls.cancelled).toEqual(['w1']);
    // The fake leaves an index that never finished a build in `error`; the re-read shows it.
    await waitFor(() => expect(chip()).toHaveTextContent('Error'));
  });

  it('rebuilds and turns off a built index', async () => {
    const ipc = createFakeIndexIpc({ statuses: { w1: READY } });
    const user = userEvent.setup();
    mount(ipc);
    const dialog = await openDialog(user);
    expect(within(dialog).getByTestId('index-summary')).toHaveTextContent('120 files, 12400 entities and 3000 relations.');

    await user.click(within(dialog).getByRole('button', { name: 'Rebuild' }));
    expect(ipc.calls.refreshes).toEqual([{ workspaceId: 'w1', full: true }]);
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Cancel build' })).toBeInTheDocument());

    await user.click(within(dialog).getByRole('button', { name: 'Turn off' }));
    expect(ipc.calls.disabled).toEqual(['w1']);
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Turn on' })).toBeInTheDocument());
    expect(chip()).toHaveTextContent('Off');
  });

  it('removes only after the confirmation, and keeps it on "Keep it"', async () => {
    const ipc = createFakeIndexIpc({ statuses: { w1: READY } });
    const user = userEvent.setup();
    mount(ipc);
    const dialog = await openDialog(user);

    await user.click(within(dialog).getByRole('button', { name: 'Remove index…' }));
    expect(within(dialog).getByTestId('index-remove-confirm')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Keep it' }));
    expect(ipc.calls.removed).toEqual([]);

    await user.click(within(dialog).getByRole('button', { name: 'Remove index…' }));
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(ipc.calls.removed).toEqual(['w1']);
    await waitFor(() => expect(chip()).toHaveTextContent('Off'));
  });

  it('says why an action was refused, and switches to the policy notice when the policy changed', async () => {
    const ipc = createFakeIndexIpc({ statuses: { w1: READY } });
    const user = userEvent.setup();
    mount(ipc);
    const dialog = await openDialog(user);

    ipc.failNext('refresh', 'index_busy', 'busy');
    await user.click(within(dialog).getByRole('button', { name: 'Rebuild' }));
    expect(await within(dialog).findByText(/already being built/)).toBeInTheDocument();

    ipc.setPolicyAllowed(false);
    await user.click(within(dialog).getByRole('button', { name: 'Rebuild' }));
    expect(await within(dialog).findByTestId('index-policy-off')).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Rebuild' })).toBeNull();
  });
});

describe('WorkspaceIndexControl without a host', () => {
  it('renders nothing', () => {
    const client = new QueryClient();
    renderWithTheme(
      <QueryClientProvider client={client}>
        <WorkspaceIndexControl workspaceId="w1" name="app" />
      </QueryClientProvider>,
    );
    expect(screen.queryByTestId('index-status-chip')).toBeNull();
  });
});

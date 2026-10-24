import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createFakeWorktreeIpc, fakeWorktreeEntry, type FakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { WorktreesPanel } from './WorktreesPanel';

function renderPanel(ipc: FakeWorktreeIpc): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <WorktreesPanel workspaceId="w1" workspaceName="repo" ipc={ipc} />
    </QueryClientProvider>,
  );
}

const clean = fakeWorktreeEntry({ thread_uuid: 'u-clean', branch: 'elitea/aaaaaaaa', ahead: 2 });
const dirty = fakeWorktreeEntry({ thread_uuid: 'u-dirty', branch: 'elitea/bbbbbbbb', ahead: 1, dirty: 3 });

describe('WorktreesPanel', () => {
  it('lists each task with its branch, ahead and uncommitted counts', async () => {
    renderPanel(createFakeWorktreeIpc({ entries: [clean, dirty] }));
    const rows = await screen.findAllByTestId('worktree-row');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('elitea/aaaaaaaa');
    expect(rows[0]).toHaveTextContent('2 ahead');
    expect(rows[0]).not.toHaveTextContent('uncommitted');
    expect(rows[1]).toHaveTextContent('1 ahead · 3 uncommitted');
  });

  it('removes a clean task without asking', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({ entries: [clean] });
    renderPanel(ipc);
    const row = await screen.findByTestId('worktree-row');
    await user.click(within(row).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(screen.queryByTestId('worktree-row')).toBeNull());
    expect(ipc.calls.remove).toEqual([['w1', 'u-clean', false]]);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('asks before removing uncommitted work, then retries with force', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({ entries: [dirty] });
    renderPanel(ipc);
    const row = await screen.findByTestId('worktree-row');
    await user.click(within(row).getByRole('button', { name: 'Remove' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('never committed');
    expect(screen.getByTestId('worktree-row')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Remove anyway' }));
    await waitFor(() => expect(screen.queryByTestId('worktree-row')).toBeNull());
    expect(ipc.calls.remove).toEqual([
      ['w1', 'u-dirty', false],
      ['w1', 'u-dirty', true],
    ]);
  });

  it('keeps uncommitted work when the person backs out', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({ entries: [dirty] });
    renderPanel(ipc);
    await user.click(within(await screen.findByTestId('worktree-row')).getByRole('button', { name: 'Remove' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Keep it' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(ipc.calls.remove).toEqual([['w1', 'u-dirty', false]]);
    expect(screen.getByTestId('worktree-row')).toBeInTheDocument();
  });

  it('shows the conflicts of a preview and cannot apply: the work stays on its branch', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({
      entries: [clean],
      preflight: { mode: 'conflicts', applied_paths: [], conflict_paths: ['src/a.ts'], skipped_paths: ['.env'], branch: 'elitea/aaaaaaaa' },
    });
    renderPanel(ipc);
    await user.click(within(await screen.findByTestId('worktree-row')).getByRole('button', { name: 'Merge back' }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByTestId('worktree-merge-mode')).toHaveTextContent('The work stays on its branch, elitea/aaaaaaaa.');
    expect(within(dialog).getByTestId('worktree-conflict-paths')).toHaveTextContent('src/a.ts');
    expect(within(dialog).getByTestId('worktree-skipped-paths')).toHaveTextContent('.env');
    expect(within(dialog).getByRole('button', { name: 'Apply' })).toBeDisabled();
    expect(ipc.calls.apply).toEqual([]);
  });

  it('applies a clean preview, then undoes it from the checkpoint', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({
      entries: [clean],
      preflight: { mode: 'merge', applied_paths: ['src/a.ts', 'src/b.ts'], conflict_paths: [], skipped_paths: [], branch: 'elitea/aaaaaaaa' },
    });
    renderPanel(ipc);
    await user.click(within(await screen.findByTestId('worktree-row')).getByRole('button', { name: 'Merge back' }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByTestId('worktree-applied-paths')).toHaveTextContent('src/b.ts');
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    expect(await within(dialog).findByTestId('worktree-merge-applied')).toHaveTextContent('2 files written, 0 deleted');
    expect(ipc.calls.apply).toEqual([['pf-1']]);

    await user.click(within(dialog).getByRole('button', { name: 'Undo merge' }));
    expect(await within(dialog).findByText('Undone: 2 files restored to before the merge.')).toBeInTheDocument();
    expect(ipc.calls.undo).toEqual([['cp-1']]);
  });

  it('asks again before an undo that would revert what changed after the merge', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc({
      entries: [clean],
      preflight: { mode: 'merge', applied_paths: ['src/a.ts'], conflict_paths: [], skipped_paths: [], branch: 'elitea/aaaaaaaa' },
    });
    renderPanel(ipc);
    await user.click(within(await screen.findByTestId('worktree-row')).getByRole('button', { name: 'Merge back' }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByTestId('worktree-applied-paths');
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    await within(dialog).findByTestId('worktree-merge-applied');

    ipc.failNext('undo', 'undo_not_latest', 'The folder changed after this merge.');
    await user.click(within(dialog).getByRole('button', { name: 'Undo merge' }));
    expect(await within(dialog).findByText('The folder changed after this merge.')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Undo anyway' }));
    expect(await within(dialog).findByText('Undone: 1 files restored to before the merge.')).toBeInTheDocument();
    expect(ipc.calls.undo).toEqual([['cp-1'], ['cp-1', true]]);
  });

  it('makes a new parallel task for a fresh thread UUID and lists it', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc();
    renderPanel(ipc);
    await user.click(await screen.findByRole('button', { name: 'New parallel task' }));
    expect(await screen.findByTestId('worktree-row')).toHaveTextContent('elitea/');
    expect(ipc.calls.create).toHaveLength(1);
    const [workspaceId, uuid] = ipc.calls.create[0] as [string, string];
    expect(workspaceId).toBe('w1');
    expect(uuid).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  });

  it('says why a task could not be made', async () => {
    const user = userEvent.setup();
    const ipc = createFakeWorktreeIpc();
    ipc.failNext('create', 'unsafe_repository');
    renderPanel(ipc);
    await user.click(await screen.findByRole('button', { name: 'New parallel task' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Git does not trust this repository');
  });
});

/**
 * Take back, driven against the in-memory host: the fast path (apply, then
 * undo), conflicts (worktree or discard), a cloud turn still running, and
 * the failures that leave the thread as it was.
 */
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createFakeHandoffIpc, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';

import { RECLAIM_POLL_MS } from '../model/useTakeBack';
import { renderWithClient } from './testing';
import { TakeBackDialog } from './TakeBackDialog';

function mount(ipc: FakeHandoffIpc, onSettled = vi.fn(), onClose = vi.fn()): { onSettled: typeof onSettled; onClose: typeof onClose } {
  renderWithClient(<TakeBackDialog open ipc={ipc} conversationUuid="u1" onClose={onClose} onSettled={onSettled} />);
  return { onSettled, onClose };
}

afterEach(() => {
  vi.useRealTimers();
});

describe('TakeBackDialog fast path', () => {
  it('reclaims, checks, and offers to apply the cloud changes; then shows the result with Undo', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ status: { state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true } });
    ipc.setPreflight({ changed_paths: ['src/main.rs', 'README.md'], skipped_paths: ['.env'], cloud_turns: 2 });
    const { onSettled } = mount(ipc);

    expect(await screen.findByText(/Your folder is as you left it. The cloud changed 2 files in 2 turns./)).toBeInTheDocument();
    expect(ipc.calls.reclaims).toEqual(['u1']);
    expect(ipc.calls.preflights).toEqual(['u1']);
    expect(screen.getByTestId('takeback-changed-paths')).toHaveTextContent('src/main.rs');
    expect(screen.getByTestId('takeback-skipped-paths')).toHaveTextContent('.env');
    // Nothing was written yet.
    expect(ipc.calls.applies).toEqual([]);
    expect(onSettled).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'Apply to my folder' }));
    const result = await screen.findByTestId('takeback-result');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'apply' }]);
    expect(result).toHaveTextContent('The cloud’s changes are in your folder as uncommitted changes.');
    expect(screen.getByTestId('takeback-applied-paths')).toHaveTextContent('src/main.rs');
    expect(screen.getByText(/elitea\/cloud\/abcd1234/)).toBeInTheDocument();
    expect(screen.getByText(/1 cloud turns were added to this thread’s history here/)).toBeInTheDocument();
    expect(screen.queryByTestId('takeback-unreleased')).toBeNull();
    expect(onSettled).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole('button', { name: 'Undo' }));
    await waitFor(() => expect(screen.getByTestId('takeback-result')).toHaveTextContent('Your folder is back to how it was before the take back.'));
    expect(ipc.calls.undos).toEqual(['w1:7']);
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
    expect(screen.queryByTestId('takeback-applied-paths')).toBeNull();
  });

  it('warns when the cloud has not acknowledged the release yet', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setApply({ released: false });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Apply to my folder' }));
    expect(await screen.findByTestId('takeback-unreleased')).toHaveTextContent('stays read-only here');
  });

  it('keeps the review open when applying fails, and says why', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('apply', 'folder_changed_since_preflight', 'moved');
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Apply to my folder' }));
    expect(await screen.findByTestId('takeback-error')).toHaveTextContent('Your folder changed since it was checked');
    // Still on the review, and applying again works.
    await user.click(screen.getByRole('button', { name: 'Apply to my folder' }));
    expect(await screen.findByTestId('takeback-result')).toBeInTheDocument();
    expect(screen.queryByTestId('takeback-error')).toBeNull();
  });
});

describe('TakeBackDialog conflicts', () => {
  const conflicts = {
    outcome: 'conflicts' as const,
    conflict_paths: ['src/lib.rs'],
    changed_paths: ['src/lib.rs', 'docs/a.md'],
    skipped_paths: [],
    modes: ['worktree', 'discard'] as ('worktree' | 'discard')[],
  };

  it('offers only a worktree or discarding, and never Apply', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    mount(ipc);
    expect(await screen.findByTestId('takeback-conflicts')).toHaveTextContent('Your folder changed since you sent this thread');
    expect(screen.getByTestId('takeback-conflict-paths')).toHaveTextContent('src/lib.rs');
    expect(screen.getByTestId('takeback-changed-paths')).toHaveTextContent('docs/a.md');
    expect(screen.getByRole('button', { name: 'Open as worktree' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Discard cloud changes' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Apply to my folder' })).toBeNull();
  });

  it('opens the cloud version as a worktree and shows where it is', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    ipc.setApply({ applied: false, outcome: 'worktree', checkpoint_id: null, worktree_path: '/Users/me/app-elitea-cloud-abcd1234', branch: 'elitea/cloud/abcd1234' });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Open as worktree' }));
    expect(await screen.findByTestId('takeback-result')).toHaveTextContent('open as a separate worktree');
    expect(screen.getByTestId('takeback-worktree-path')).toHaveTextContent('/Users/me/app-elitea-cloud-abcd1234');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'worktree' }]);
    // Nothing was written to the folder: nothing to undo.
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
  });

  it('asks before discarding, and discards only when confirmed', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    ipc.setApply({ applied: false, outcome: 'discarded', checkpoint_id: null });
    mount(ipc);

    await user.click(await screen.findByRole('button', { name: 'Discard cloud changes' }));
    expect(screen.getByTestId('takeback-discard-confirm')).toHaveTextContent('Discard the cloud’s changes?');
    expect(ipc.calls.applies).toEqual([]);
    // The safe choices are hidden while confirming; Back returns to them.
    expect(screen.queryByRole('button', { name: 'Open as worktree' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.queryByTestId('takeback-discard-confirm')).toBeNull();
    expect(screen.getByRole('button', { name: 'Open as worktree' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Discard cloud changes' }));
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    expect(await screen.findByTestId('takeback-result')).toHaveTextContent('discarded');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'discard' }]);
  });
});

describe('TakeBackDialog while a cloud turn runs', () => {
  it('waits for the thread to be handed back, then checks it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const ipc = createFakeHandoffIpc();
    ipc.setReclaim({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true });
    ipc.queueStatus({ state: 'CLOUD_RUNNING' }, { state: 'RECLAIMING', executor: 'returning' });
    mount(ipc);

    expect(await screen.findByTestId('takeback-working')).toHaveTextContent('A cloud turn is still running');
    expect(ipc.calls.preflights).toEqual([]);

    await act(() => vi.advanceTimersByTimeAsync(RECLAIM_POLL_MS));
    // Still running: nothing checked yet.
    expect(ipc.calls.statuses).toHaveLength(1);
    expect(ipc.calls.preflights).toEqual([]);

    await act(() => vi.advanceTimersByTimeAsync(RECLAIM_POLL_MS));
    expect(await screen.findByRole('button', { name: 'Apply to my folder' })).toBeInTheDocument();
    expect(ipc.calls.preflights).toEqual(['u1']);
  });

  it('stops waiting when the dialog is closed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const ipc = createFakeHandoffIpc();
    ipc.setReclaim({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true });
    ipc.setStatus({ state: 'CLOUD_RUNNING' });
    const view = renderWithClient(<TakeBackDialog open ipc={ipc} conversationUuid="u1" onClose={vi.fn()} onSettled={vi.fn()} />);
    await screen.findByTestId('takeback-working');
    view.unmount();
    await act(() => vi.advanceTimersByTimeAsync(RECLAIM_POLL_MS * 3));
    expect(ipc.calls.statuses).toEqual([]);
  });

  it('says there is nothing to take back when the thread was released meanwhile', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const ipc = createFakeHandoffIpc();
    ipc.setReclaim({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true });
    ipc.setStatus({ state: 'LOCAL', executor: 'local' });
    const { onSettled } = mount(ipc);
    await screen.findByTestId('takeback-working');
    await act(() => vi.advanceTimersByTimeAsync(RECLAIM_POLL_MS));
    expect(await screen.findByText(/no longer in the cloud/)).toBeInTheDocument();
    expect(onSettled).toHaveBeenCalled();
  });
});

describe('TakeBackDialog failures', () => {
  it.each([
    ['reclaim', 'handoff_unknown', 'was not sent from this computer'],
    ['preflight', 'integrity_failed', 'failed their integrity check'],
    ['preflight', 'handoff_not_reclaimed', 'has not been handed back by the cloud yet'],
  ] as const)('shows a failed %s (%s) and lets the person try again', async (command, code, text) => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext(command, code, 'raw');
    mount(ipc);
    const error = await screen.findByTestId('takeback-error');
    expect(error).toHaveTextContent(text);
    expect(ipc.calls.applies).toEqual([]);

    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('button', { name: 'Apply to my folder' })).toBeInTheDocument();
    expect(screen.queryByTestId('takeback-error')).toBeNull();
  });

  it('keeps Undo available when undoing fails', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Apply to my folder' }));
    await screen.findByTestId('takeback-result');
    ipc.failNext('undo', 'workspace_busy', 'busy');
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(await screen.findByTestId('takeback-error')).toHaveTextContent('An agent is still working in this folder');
    expect(within(screen.getByRole('dialog')).getByRole('button', { name: 'Undo' })).toBeEnabled();
  });
});

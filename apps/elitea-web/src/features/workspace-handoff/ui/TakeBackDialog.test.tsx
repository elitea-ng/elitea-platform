/**
 * Take back, driven against the in-memory host: the fast path (apply, then
 * undo), a clean merge, conflicts (markers, worktree or discard), picking a
 * cloud turn, a cloud turn still running, and the failures that leave the
 * thread as it was.
 */
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ChangedFilesCard } from '@/features/workspace';
import type { Preflight } from '@/shared/desktop/handoffIpc';
import { createFakeHandoffIpc, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

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
    ipc.setPreflight({
      changed_paths: [
        { path: 'src/main.rs', change: 'modified', from: null },
        { path: 'README.md', change: 'modified', from: null },
      ],
      skipped_paths: [{ path: '.env', reason: 'excluded' }],
      cloud_turns: 2,
    });
    const { onSettled } = mount(ipc);

    expect(await screen.findByText(/Your folder is as you left it. The cloud changed 2 files in 2 turns./)).toBeInTheDocument();
    expect(ipc.calls.reclaims).toEqual(['u1']);
    expect(ipc.calls.preflights).toEqual(['u1']);
    expect(screen.getByTestId('takeback-changed-paths')).toHaveTextContent('src/main.rs');
    expect(screen.getByTestId('takeback-skipped-paths')).toHaveTextContent('.env (kept on this computer)');
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
  const conflicts: Partial<Preflight> = {
    outcome: 'conflicts',
    conflict_paths: ['src/lib.rs', 'assets/logo.png'],
    changed_paths: [
      { path: 'src/lib.rs', change: 'modified', from: null },
      { path: 'docs/a.md', change: 'renamed', from: 'docs/old.md' },
    ],
    skipped_paths: [
      { path: 'assets/logo.png', reason: 'binary_conflict' },
      { path: 'secrets/prod.key', reason: 'path_deny' },
    ],
    result_tree: null,
    modes: ['apply_with_markers', 'worktree', 'discard'],
  };

  it('offers markers, a worktree or discarding, and never a plain Apply; lists why paths are skipped', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    mount(ipc);
    expect(await screen.findByTestId('takeback-conflicts')).toHaveTextContent('Your folder and the cloud changed the same lines');
    expect(screen.getByTestId('takeback-conflict-paths')).toHaveTextContent('src/lib.rs');
    expect(screen.getByTestId('takeback-changed-paths')).toHaveTextContent('docs/old.md → docs/a.md');
    const skipped = screen.getByTestId('takeback-skipped-paths');
    expect(skipped).toHaveTextContent('assets/logo.png (binary file changed on both sides; your version is kept)');
    expect(skipped).toHaveTextContent('secrets/prod.key (blocked by policy)');
    expect(screen.getByRole('button', { name: 'Apply with conflict markers' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Open as worktree' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Discard cloud changes' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Apply to my folder' })).toBeNull();
  });

  it('applies with conflict markers and lists the files to resolve, never the binary one', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    ipc.setApply({ outcome: 'applied_with_markers' });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Apply with conflict markers' }));
    expect(await screen.findByTestId('takeback-result')).toHaveTextContent('conflict markers to resolve');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'apply_with_markers' }]);
    const markers = screen.getByTestId('takeback-marker-paths');
    expect(markers).toHaveTextContent('src/lib.rs');
    expect(markers).not.toHaveTextContent('assets/logo.png');
    expect(screen.getByRole('button', { name: 'Undo' })).toBeInTheDocument();
  });

  it('opens the cloud version as a worktree and shows where it is', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight(conflicts);
    ipc.setApply({ applied: false, outcome: 'worktree', checkpoint_id: null, worktree_path: '/Users/me/app-elitea-cloud-abcd1234', branch: 'elitea/cloud/abcd1234', copied: ['.env', 'keys/dev.pem'] });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Open as worktree' }));
    expect(await screen.findByTestId('takeback-result')).toHaveTextContent('open as a separate worktree');
    expect(screen.getByTestId('takeback-worktree-path')).toHaveTextContent('/Users/me/app-elitea-cloud-abcd1234');
    expect(screen.getByTestId('takeback-worktree')).toHaveTextContent('to work in it as a workspace');
    expect(screen.getByTestId('takeback-copied-paths')).toHaveTextContent('.env');
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

describe('TakeBackDialog clean merge', () => {
  it('says both sides are kept and applies them', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight({ outcome: 'clean_merge' });
    mount(ipc);
    expect(await screen.findByTestId('takeback-clean-merge')).toHaveTextContent('Applying keeps both');
    expect(screen.queryByRole('button', { name: 'Apply with conflict markers' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Apply to my folder' }));
    expect(await screen.findByTestId('takeback-result')).toHaveTextContent('uncommitted changes');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'apply' }]);
  });
});

describe('TakeBackDialog turns', () => {
  it('takes back the latest turn by default, and up to an earlier one when picked', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight({ cloud_turns: 3 });
    mount(ipc);
    const picker = await screen.findByRole('combobox', { name: 'Take back up to' });
    expect(picker).toHaveTextContent('Turn 3 (latest)');
    expect(ipc.calls.preflightTurns).toEqual([undefined]);
    expect(screen.queryByTestId('takeback-turns-after')).toBeNull();

    await user.click(picker);
    await user.click(await screen.findByRole('option', { name: 'Turn 2' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Take back up to' })).toHaveTextContent('Turn 2'));
    expect(ipc.calls.preflightTurns).toEqual([undefined, 2]);
    expect(screen.getByText(/The cloud changed 1 files in 2 turns/)).toBeInTheDocument();
    // Turn 3 is not taken back: the review says so before anything is applied.
    expect(screen.getByTestId('takeback-turns-after')).toHaveTextContent('Turns 3 to 3 are not taken back');
    expect(screen.getByTestId('takeback-turns-after')).toHaveTextContent('deleted with the cloud copy');

    ipc.setApply({ imported_turns: 2, turns_not_taken: 1, rest_branch: 'elitea/cloud/abcd1234-after-turn-2' });
    await user.click(screen.getByRole('button', { name: 'Apply to my folder' }));
    await screen.findByTestId('takeback-result');
    expect(ipc.calls.applies).toEqual([{ preflightId: 'pf-1', mode: 'apply', upToTurn: 2 }]);
    expect(screen.getByTestId('takeback-rest-branch')).toHaveTextContent('elitea/cloud/abcd1234-after-turn-2');
  });

  it('shows no picker for a single cloud turn', async () => {
    const ipc = createFakeHandoffIpc();
    mount(ipc);
    await screen.findByRole('button', { name: 'Apply to my folder' });
    expect(screen.queryByRole('combobox', { name: 'Take back up to' })).toBeNull();
  });

  it('keeps the previous review when the picked turn cannot be checked', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight({ cloud_turns: 2 });
    mount(ipc);
    const picker = await screen.findByRole('combobox', { name: 'Take back up to' });
    ipc.failNext('preflight', 'integrity_failed', 'bad');
    await user.click(picker);
    await user.click(await screen.findByRole('option', { name: 'Turn 1' }));
    expect(await screen.findByTestId('takeback-error')).toHaveTextContent('failed their integrity check');
    expect(screen.getByRole('combobox', { name: 'Take back up to' })).toHaveTextContent('Turn 2 (latest)');
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

describe('TakeBackDialog diffs', () => {
  const mountWithWorkspace = (ipc: FakeHandoffIpc): void => {
    const workspaceIpc = createFakeWorkspaceIpc();
    // As the page wires it: the Workspace view's changed-files card, read-only.
    renderWithClient(
      <TakeBackDialog
        open
        ipc={ipc}
        conversationUuid="u1"
        onClose={vi.fn()}
        onSettled={vi.fn()}
        renderFiles={(files, key) => <ChangedFilesCard ipc={workspaceIpc} turnId={key} files={files} />}
      />,
    );
  };

  it('shows the per-file diffs of what the take back writes, read-only', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight({
      changed_paths: [
        { path: 'src/main.rs', change: 'modified', from: null },
        { path: 'docs/new.md', change: 'added', from: null },
      ],
      files: [
        { path: 'src/main.rs', status: 'modified', added: 1, removed: 1, diff: '--- a/src/main.rs\n+++ b/src/main.rs\n@@ -1 +1 @@\n-fn main() {}\n+fn main() { run() }\n' },
        { path: 'docs/new.md', status: 'added', added: 2, removed: 0, diff: '' },
      ],
    });
    mountWithWorkspace(ipc);

    const diffs = await screen.findByTestId('takeback-diffs');
    const rows = within(diffs).getAllByTestId('changed-file');
    expect(rows.map((row) => within(row).getByText(/src\/main.rs|docs\/new.md/).textContent)).toEqual(['src/main.rs', 'docs/new.md']);
    expect(rows[0]).toHaveTextContent('+1 -1');
    // Read-only: nothing to revert or undo here.
    expect(within(diffs).queryByRole('button', { name: /Revert|Undo turn/ })).toBeNull();
    await user.click(within(rows[0] as HTMLElement).getByRole('button', { name: 'Show diff' }));
    expect(await within(diffs).findByText(/run\(\)/)).toBeInTheDocument();
    expect(screen.queryByTestId('takeback-diffs-truncated')).toBeNull();
    expect(screen.queryByTestId('takeback-changed-paths')).toBeNull();
  });

  it('says when some diffs were left out', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.setPreflight({ diffs_truncated: true });
    mountWithWorkspace(ipc);
    expect(await screen.findByTestId('takeback-diffs-truncated')).toHaveTextContent('too large to show here');
  });
});

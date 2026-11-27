import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { ChangedFile } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ChangedFilesCard } from './ChangedFilesCard';

const DIFF = ['--- a/src/a.ts', '+++ b/src/a.ts', '@@ -1 +1 @@', '-old', '+new'].join('\n');

function setup(
  files: ChangedFile[] = [
    { path: 'src/a.ts', status: 'modified' as const, added: 1, removed: 1, diff: DIFF },
    { path: 'src/b.ts', status: 'added' as const, added: 4, removed: 0, diff: '' },
  ],
  { latest = true }: { latest?: boolean } = {},
) {
  const ipc = createFakeWorkspaceIpc();
  ipc.setChanges('t1', { files, latest });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = renderWithTheme(
    <QueryClientProvider client={client}>
      <ChangedFilesCard ipc={ipc} turnId="t1" />
    </QueryClientProvider>,
  );
  return { ipc, user: userEvent.setup(), ...view };
}

describe('ChangedFilesCard', () => {
  it('lists each file with its status and +/- counts, and totals them', async () => {
    setup();
    const rows = await screen.findAllByTestId('changed-file');
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText('Modified')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByText('+1 -1')).toBeInTheDocument();
    expect(within(rows[1] as HTMLElement).getByText('Added')).toBeInTheDocument();
    expect(within(rows[1] as HTMLElement).getByText('+4 -0')).toBeInTheDocument();
    expect(screen.getByText('2 files, +5 -1')).toBeInTheDocument();
  });

  it('shows nothing when the turn changed nothing', async () => {
    const { container } = setup([]);
    await waitFor(() => expect(container.querySelector('[data-testid="changed-files-card"]')).toBeNull());
  });

  it('expands a unified diff with added and removed lines, only for files that carry one', async () => {
    const { user } = setup();
    const rows = await screen.findAllByTestId('changed-file');
    expect(within(rows[1] as HTMLElement).queryByRole('button', { name: 'Show diff' })).toBeNull();

    await user.click(within(rows[0] as HTMLElement).getByRole('button', { name: 'Show diff' }));
    const added = document.querySelector('[data-diff-kind="added"]');
    const removed = document.querySelector('[data-diff-kind="removed"]');
    expect(added).toHaveTextContent('+ new');
    expect(removed).toHaveTextContent('- old');
  });

  it('asks before undoing the turn, and does nothing if the user keeps the changes', async () => {
    const { ipc, user } = setup();
    await screen.findAllByTestId('changed-file');

    await user.click(screen.getByRole('button', { name: 'Undo turn' }));
    const dialog = await screen.findByRole('dialog', { name: 'Undo this turn?' });
    expect(ipc.calls.restores).toEqual([]);

    await user.click(within(dialog).getByRole('button', { name: 'Keep changes' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(ipc.calls.restores).toEqual([]);
  });

  it('restores the whole turn (no path) once confirmed, and reports what came back', async () => {
    const { ipc, user } = setup();
    await screen.findAllByTestId('changed-file');

    await user.click(screen.getByRole('button', { name: 'Undo turn' }));
    const dialog = await screen.findByRole('dialog', { name: 'Undo this turn?' });
    await user.click(within(dialog).getByRole('button', { name: 'Undo turn' }));

    await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 't1' }]));
    expect(await screen.findByText('Restored 2 files.')).toBeInTheDocument();
  });

  it('reverts a single file by path, without a confirmation', async () => {
    const { ipc, user } = setup();
    await screen.findAllByTestId('changed-file');

    await user.click(screen.getByRole('button', { name: 'Revert src/b.ts' }));
    await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 't1', path: 'src/b.ts' }]));
    expect(await screen.findByText('Restored 1 files.')).toBeInTheDocument();
  });

  it('says why a turn that ran without a checkpoint cannot be undone', async () => {
    const { ipc, user } = setup();
    ipc.failNext('restore', 'no_checkpoint', 'This turn ran without a checkpoint (too many files), so its changes cannot be undone here.');
    await screen.findAllByTestId('changed-file');

    await user.click(screen.getByRole('button', { name: 'Revert src/b.ts' }));
    // The host's own words (they name the reason), not the generic failure.
    expect(await screen.findByText('This turn ran without a checkpoint (too many files), so its changes cannot be undone here.')).toBeInTheDocument();
  });

  it('an older turn offers to restore the folder, lists what the host says would be lost, and asks with the confirmation', async () => {
    const { ipc, user } = setup([{ path: 'src/a.ts', status: 'modified', added: 1, removed: 1, diff: DIFF }], { latest: false });
    ipc.setPreview('t1', { restored: ['src/a.ts'], deleted: ['src/later.ts', 'notes-by-me.md'] });
    await screen.findAllByTestId('changed-file');
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Undo turn' })).toBeNull());

    await user.click(await screen.findByRole('button', { name: 'Restore folder to before this turn' }));
    const dialog = await screen.findByRole('dialog', { name: 'Restore the folder to before this turn?' });
    const listed = await within(dialog).findByTestId('restore-folder-preview');
    expect(within(listed).getByText('src/a.ts')).toBeInTheDocument();
    expect(within(listed).getByText('src/later.ts (deleted)')).toBeInTheDocument();
    expect(within(listed).getByText('notes-by-me.md (deleted)')).toBeInTheDocument();
    expect(within(dialog).getByText('…and any edits you made since are lost.')).toBeInTheDocument();
    expect(ipc.calls.restores).toEqual([]);

    await user.click(within(dialog).getByRole('button', { name: 'Restore folder' }));
    await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 't1', confirmOlder: true }]));
    // Undone now: nothing more to undo or revert.
    expect(await screen.findByText('Undone')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('button', { name: /Revert|Undo turn|Restore folder/ })).toBeNull());
  });

  it('says why one file of an older turn was not reverted', async () => {
    const { ipc, user } = setup([{ path: 'src/a.ts', status: 'modified', added: 1, removed: 1, diff: DIFF }], { latest: false });
    ipc.failNext('restore', 'file_changed_since', 'host words');
    await user.click(await screen.findByRole('button', { name: 'Revert src/a.ts' }));
    expect(await screen.findByText('This file changed after this turn (a later turn or your own edit), so it was not reverted.')).toBeInTheDocument();
    expect(ipc.calls.restores).toEqual([{ turnId: 't1', path: 'src/a.ts' }]);
  });

  it('shows a recorded turn read-only, without asking the host or offering undo', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.failNext('turnChanges', 'turn_unknown', 'not kept');
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <ChangedFilesCard ipc={ipc} turnId="old" files={[{ path: 'src/a.ts', status: 'modified', added: 1, removed: 1, diff: DIFF }]} undoRequest={1} />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('src/a.ts')).toBeInTheDocument();
    expect(screen.getByText('1 file, +1 -1')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Undo turn' })).toBeNull();
    expect(screen.queryByRole('button', { name: /Revert/ })).toBeNull();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('button', { name: 'Show diff' })).toBeInTheDocument();
    // The failure queued for turn_changes was never consumed: the host was not asked.
    await expect(ipc.turnChanges('old')).rejects.toMatchObject({ code: 'turn_unknown' });
  });

  it('says what undo does not cover: files the checkpoint left out, and commands it could not list', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.setChanges('t1', {
      files: [{ path: 'notes.md', status: 'modified', added: 1, removed: 0, diff: DIFF }],
      latest: true,
      not_saved: { too_large: ['a.mov', 'b.mov', 'c.mov', 'd.mov', 'e.mov', 'f.mov'], too_large_count: 8, placeholders: ['cloud.pdf'], placeholder_count: 1 },
      commands_unlisted: true,
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <ChangedFilesCard ipc={ipc} turnId="t1" />
      </QueryClientProvider>,
    );
    expect(await screen.findByTestId('changes-not-saved')).toHaveTextContent('a.mov, b.mov, c.mov, d.mov, e.mov and 3 more');
    expect(screen.getByTestId('changes-placeholders')).toHaveTextContent('cloud.pdf');
    expect(screen.getByTestId('changes-commands-unlisted')).toBeInTheDocument();
  });

  it('says nothing about coverage when undo covers every file', async () => {
    setup();
    await screen.findAllByTestId('changed-file');
    expect(screen.queryByTestId('changes-coverage')).toBeNull();
  });

  it('shows a command-made binary file by its sizes, not empty line counts', async () => {
    setup([{ path: 'deck.pptx', status: 'deleted' as const, added: 0, removed: 0, diff: '', binary: true, size_before: 532, command: 'rm deck.pptx' }]);
    const row = await screen.findByTestId('changed-file');
    expect(within(row).getByTestId('change-sizes')).toHaveTextContent('532 bytes');
    expect(within(row).queryByText('+0 -0')).toBeNull();
    expect(within(row).getByTestId('change-command')).toHaveTextContent('rm deck.pptx');
  });
});

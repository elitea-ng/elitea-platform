import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ChangedFilesCard } from './ChangedFilesCard';

const DIFF = ['--- a/src/a.ts', '+++ b/src/a.ts', '@@ -1 +1 @@', '-old', '+new'].join('\n');

function setup(files = [
  { path: 'src/a.ts', status: 'modified' as const, added: 1, removed: 1, diff: DIFF },
  { path: 'src/b.ts', status: 'added' as const, added: 4, removed: 0, diff: '' },
]) {
  const ipc = createFakeWorkspaceIpc();
  ipc.setChanges('t1', { files });
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
});

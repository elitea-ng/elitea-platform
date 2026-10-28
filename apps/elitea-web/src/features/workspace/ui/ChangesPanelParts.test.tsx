import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ChangedFile } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ChangedFilesCard, type ChangedFilesCardProps } from './ChangedFilesCard';
import { splitPath } from './ChangesPanelParts';

const DIFF = ['--- a/src/app/a.ts', '+++ b/src/app/a.ts', '@@ -1 +1 @@', '-old', '+new'].join('\n');
const FILES: ChangedFile[] = [
  { path: 'src/app/a.ts', status: 'modified', added: 1, removed: 1, diff: DIFF },
  { path: 'README.md', status: 'added', added: 4, removed: 0, diff: '' },
  { path: 'old/gone.rs', status: 'deleted', added: 0, removed: 7, diff: '' },
];

function setup(props: Partial<ChangedFilesCardProps> = {}, { latest = true }: { latest?: boolean } = {}) {
  const ipc = createFakeWorkspaceIpc();
  ipc.setChanges('t1', { files: FILES, latest });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <ChangedFilesCard ipc={ipc} turnId="t1" variant="panel" label="Fix the parser and update the docs" {...props} />
    </QueryClientProvider>,
  );
  return { ipc, user: userEvent.setup() };
}

describe('splitPath', () => {
  it('splits the file name from its folder', () => {
    expect(splitPath('src/app/main.rs')).toEqual({ name: 'main.rs', dir: 'src/app' });
    expect(splitPath('README.md')).toEqual({ name: 'README.md', dir: '' });
  });
});

describe('ChangedFilesCard, panel variant', () => {
  it('heads the turn with its prompt on one line and the totals, and lists one dense row per file', async () => {
    setup();
    const toggle = await screen.findByTestId('changes-turn-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle).toHaveAttribute('title', 'Fix the parser and update the docs');
    expect(toggle).toHaveTextContent('3 files');
    expect(within(toggle).getByTestId('change-counts')).toHaveTextContent('+5−8');

    const rows = screen.getAllByTestId('changed-file');
    expect(rows).toHaveLength(3);
    const first = rows[0] as HTMLElement;
    expect(within(first).getByText('a.ts')).toBeInTheDocument();
    expect(within(first).getByText('src/app')).toBeInTheDocument();
    expect(within(first).getByText('M')).toHaveAttribute('aria-label', 'Modified');
    expect(within(rows[1] as HTMLElement).getByText('A')).toBeInTheDocument();
    expect(within(rows[2] as HTMLElement).getByText('D')).toBeInTheDocument();
    // No pill, no permanent "Show diff" button.
    expect(screen.queryByText('Modified')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Show diff' })).toBeNull();
  });

  it('a click on a row with a diff toggles it inline; a row without one is not a button', async () => {
    const { user } = setup();
    const toggle = await screen.findByRole('button', { name: 'Diff of src/app/a.ts' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(document.querySelector('[data-diff-kind="added"]')).toHaveTextContent('+ new');
    expect(screen.queryByRole('button', { name: 'Diff of README.md' })).toBeNull();

    await user.click(toggle);
    await waitFor(() => expect(document.querySelector('[data-diff-kind="added"]')).toBeNull());
  });

  it('folds and unfolds the turn from its header; an older turn can start folded', async () => {
    const { user } = setup({ defaultExpanded: false });
    const toggle = await screen.findByTestId('changes-turn-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryAllByTestId('changed-file')).toHaveLength(0);
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(await screen.findAllByTestId('changed-file')).toHaveLength(3);
  });

  // The actions show on hover or keyboard focus (jsdom has no :hover): they are pressed from the keyboard here.
  it('keeps per-file actions and undo reachable as labelled icon buttons', async () => {
    const reveal = vi.fn();
    const { ipc, user } = setup({
      fileActions: (file) => (
        <button type="button" aria-label={`Reveal ${file.path} in Finder`} onClick={reveal}>
          r
        </button>
      ),
    });
    await screen.findAllByTestId('changed-file');
    screen.getByRole('button', { name: 'Reveal README.md in Finder' }).focus();
    await user.keyboard('{Enter}');
    expect(reveal).toHaveBeenCalledTimes(1);

    screen.getByRole('button', { name: 'Revert README.md' }).focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(ipc.calls.restores).toEqual([{ turnId: 't1', path: 'README.md' }]));

    screen.getByRole('button', { name: 'Undo turn' }).focus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('dialog', { name: 'Undo this turn?' })).toBeInTheDocument();
  });

  it('offers "Restore folder" for an older turn, and nothing for a recorded one', async () => {
    setup({}, { latest: false });
    expect(await screen.findByRole('button', { name: 'Restore folder to before this turn' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Undo turn' })).toBeNull();
  });

  it('a recorded turn is read-only and falls back to "This turn" without a prompt', async () => {
    setup({ files: FILES, label: '' });
    const toggle = await screen.findByTestId('changes-turn-toggle');
    expect(toggle).toHaveTextContent('This turn');
    expect(screen.queryByRole('button', { name: /Undo turn|Restore folder|Revert/ })).toBeNull();
  });
});

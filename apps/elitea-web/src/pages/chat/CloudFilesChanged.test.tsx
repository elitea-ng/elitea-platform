/**
 * Files changed of a cloud thread: the cumulative and per-turn lists, a
 * binary file without a diff, a file's diff on demand, and the notes for
 * hidden, unlisted and unrecorded changes. Bodies are the contract's own
 * examples (the generated mocks), overridden only where a case needs it.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetWorkspaceHandoffChangesMockHandler,
  getGetWorkspaceHandoffChangesResponseMock,
  getGetWorkspaceHandoffDiffMockHandler,
} from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { WorkspaceHandoffChanges } from '@/shared/api/generated/model';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import { changedCount, scopeView, turnNumber } from './cloudChanges';
import CloudFilesChanged from './CloudFilesChanged';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';

function mount(): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <CloudFilesChanged projectId="42" conversationUuid={UUID} open onClose={() => undefined} />
    </QueryClientProvider>,
  );
}

function rowOf(path: string): HTMLElement {
  const row = screen.getAllByTestId('cloud-changed-file').find((element) => element.textContent?.includes(path));
  if (row === undefined) throw new Error(`no row for ${path}`);
  return row;
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('CloudFilesChanged', () => {
  it('lists what changed since the handoff, with status and line counts', async () => {
    server.use(getGetWorkspaceHandoffChangesMockHandler());
    mount();
    expect(await screen.findByTestId('cloud-files-summary')).toHaveTextContent('3 files, +14 -3');
    const parser = rowOf('src/parser.rs');
    expect(parser).toHaveTextContent('Modified');
    expect(parser).toHaveTextContent('+12 -3');
    expect(rowOf('docs/parser.md')).toHaveTextContent('Added');
    expect(screen.queryByTestId('cloud-files-hidden')).toBeNull();
    expect(screen.queryByTestId('cloud-files-incomplete')).toBeNull();
  });

  it('lists a binary file without counts or a diff button', async () => {
    server.use(getGetWorkspaceHandoffChangesMockHandler());
    mount();
    await screen.findByTestId('cloud-files-summary');
    const logo = rowOf('assets/logo.png');
    expect(logo).toHaveTextContent('Binary');
    expect(logo).not.toHaveTextContent('+');
    expect(within(logo).queryByRole('button', { name: 'Show diff' })).toBeNull();
  });

  it('reads and renders a file diff when asked', async () => {
    const asked: string[] = [];
    server.use(
      getGetWorkspaceHandoffChangesMockHandler(),
      getGetWorkspaceHandoffDiffMockHandler((info) => {
        asked.push(new URL(info.request.url).search);
        return { commit: '9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3', scope: 'cumulative', path: 'docs/parser.md', diff: '@@ -0,0 +1 @@\n+# Parser\n' };
      }),
    );
    const user = userEvent.setup();
    mount();
    await screen.findByTestId('cloud-files-summary');
    await user.click(within(rowOf('docs/parser.md')).getByRole('button', { name: 'Show diff' }));
    expect(await screen.findByTestId('cloud-file-diff')).toHaveTextContent('# Parser');
    const query = new URLSearchParams(asked[0]);
    expect(query.get('scope')).toBe('cumulative');
    expect(query.get('commit')).toBe('9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3');
    expect(query.get('path')).toBe('docs/parser.md');
  });

  it('switches to one turn, whose diff is read at that turn', async () => {
    const asked: string[] = [];
    server.use(
      getGetWorkspaceHandoffChangesMockHandler(),
      getGetWorkspaceHandoffDiffMockHandler((info) => {
        asked.push(new URL(info.request.url).search);
        return { commit: '9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3', scope: 'turn', path: 'src/parser.rs', diff: '@@ -1 +1 @@\n-a\n+b\n' };
      }),
    );
    const user = userEvent.setup();
    mount();
    await screen.findByTestId('cloud-files-summary');
    await user.click(screen.getByRole('combobox', { name: 'Show' }));
    await user.click(await screen.findByRole('option', { name: 'Turn 1' }));
    await user.click(within(rowOf('src/parser.rs')).getByRole('button', { name: 'Show diff' }));
    await screen.findByTestId('cloud-file-diff');
    expect(new URLSearchParams(asked[0]).get('scope')).toBe('turn');
  });

  it('says what is hidden, not listed, or not recorded', async () => {
    const example = getGetWorkspaceHandoffChangesResponseMock();
    const cumulative = example.cumulative ?? undefined;
    if (cumulative === undefined) throw new Error('the example has a cumulative set');
    const changes: WorkspaceHandoffChanges = { ...example, complete: false, cumulative: { ...cumulative, hidden_files: 2, omitted_files: 5 } };
    server.use(getGetWorkspaceHandoffChangesMockHandler(changes));
    mount();
    expect(await screen.findByTestId('cloud-files-hidden')).toHaveTextContent('2 changed files are hidden by the exclusion rules.');
    expect(screen.getByTestId('cloud-files-omitted')).toHaveTextContent('5 more changed files are not listed.');
    expect(screen.getByTestId('cloud-files-incomplete')).toBeInTheDocument();
    expect(changedCount(changes)).toBe(8);
  });

  it('says so when nothing is recorded', async () => {
    const changes: WorkspaceHandoffChanges = { ...getGetWorkspaceHandoffChangesResponseMock(), head_sha: null, cumulative: null, turns: [] };
    server.use(getGetWorkspaceHandoffChangesMockHandler(changes));
    mount();
    expect(await screen.findByTestId('cloud-files-empty')).toHaveTextContent('No changed files are recorded yet.');
    expect(scopeView(changes, { commit: '9a8b7c6d5e4f30211203f4e5d6c7b8a9f0e1d2c3' }).scope).toBe('cumulative');
  });

  it('labels turns by their number on the chain and keeps the picked turn by commit', async () => {
    const example = getGetWorkspaceHandoffChangesResponseMock();
    const [turn] = example.turns;
    if (turn === undefined) throw new Error('the example has a turn');
    const older = { ...turn, commit: '1111111111111111111111111111111111111111', number: 3 };
    const newer = { ...turn, number: 5 };
    const changes: WorkspaceHandoffChanges = { ...example, complete: false, turns: [older, newer] };
    server.use(getGetWorkspaceHandoffChangesMockHandler(changes));
    const user = userEvent.setup();
    mount();
    await screen.findByTestId('cloud-files-summary');
    await user.click(screen.getByRole('combobox', { name: 'Show' }));
    expect(await screen.findByRole('option', { name: 'Turn 3' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Turn 5' })).toBeInTheDocument();
    // A turn listed before the picked one (a gap filled) does not move the pick.
    const picked = { commit: newer.commit };
    const refilled: WorkspaceHandoffChanges = { ...changes, turns: [{ ...turn, commit: '2222222222222222222222222222222222222222', number: 1 }, older, newer] };
    expect(scopeView(refilled, picked).commit).toBe(newer.commit);
    expect(turnNumber({ ...changes, turns: [{ ...older, number: null }] }, 0)).toBe(1);
  });

  it('ties a diff button to its panel with a safe element id, whatever the path holds', async () => {
    const example = getGetWorkspaceHandoffChangesResponseMock();
    const cumulative = example.cumulative ?? undefined;
    const [first] = cumulative?.files ?? [];
    if (cumulative === undefined || first === undefined) throw new Error('the example has a file');
    const path = 'docs/a "quoted" file #1.md';
    const changes: WorkspaceHandoffChanges = { ...example, cumulative: { ...cumulative, files: [{ ...first, path }] } };
    server.use(
      getGetWorkspaceHandoffChangesMockHandler(changes),
      getGetWorkspaceHandoffDiffMockHandler(() => ({ commit: example.head_sha ?? '', scope: 'cumulative', path, diff: '@@ -0,0 +1 @@\n+x\n' })),
    );
    const user = userEvent.setup();
    mount();
    await screen.findByTestId('cloud-files-summary');
    const button = within(rowOf(path)).getByRole('button', { name: 'Show diff' });
    await user.click(button);
    await screen.findByTestId('cloud-file-diff');
    const id = button.getAttribute('aria-controls') ?? '';
    expect(id).not.toContain(' ');
    expect(document.getElementById(id)).not.toBeNull();
  });
});

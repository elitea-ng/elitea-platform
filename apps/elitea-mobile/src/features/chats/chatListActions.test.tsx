import { act, fireEvent, render, renderHook, screen } from '@testing-library/react-native';
import { AccessibilityInfo, Text } from 'react-native';

import { buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';
import { ConversationList, type ChatListItem } from '@/features/chats/ConversationList';
import { canSaveName } from '@/features/chats/RenameView';
import { UndoToast } from '@/features/chats/UndoToast';
import { UNDO_WINDOW_MS, usePendingDelete } from '@/features/chats/usePendingDelete';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const NOW = Date.parse('2026-10-07T12:00:00Z');
const summary = (id: number, name: string, updated_at: string, is_pinned?: boolean) => ({
  id,
  name,
  updated_at,
  created_at: updated_at,
  duration: -1,
  message_groups_count: 1,
  ...(is_pinned === undefined ? {} : { is_pinned }),
});

describe('buildChatRows', () => {
  it('puts pinned chats first (newest first), adds pinned ones the pages did not load, and applies queued actions', () => {
    const loaded = [summary(1, 'A', '2026-10-07T11:00:00Z'), summary(2, 'B', '2026-10-06T11:00:00Z', true), summary(3, 'C', '2026-10-05T11:00:00Z')];
    const savedPinned = [summary(2, 'B (saved)', '2026-10-06T11:00:00Z', true), summary(9, 'Old pin', '2026-09-01T11:00:00Z', true)];
    const overlay = new Map([
      [1, { name: 'A renamed' }],
      [3, { pinned: true }],
    ]);
    const rows = buildChatRows({ loaded, savedPinned, overlay, hidden: new Set([9]), now: NOW });
    expect(rows.map((r) => [r.id, r.name, r.pinned])).toEqual([
      [2, 'B', true],
      [3, 'C', true],
      [1, 'A renamed', false],
    ]);
  });
});

describe('buildSearchRows', () => {
  it('lists index hits with their passage, then loaded title matches the index lacks', () => {
    const hits = [{ id: 5, name: 'Trip', updatedAt: '2026-10-01T09:00:00Z', row: null, snippet: [{ text: 'nata', match: true }] }];
    const loaded = [summary(5, 'Trip', '2026-10-01T09:00:00Z'), summary(6, 'Nata recipes', '2026-10-02T09:00:00Z')];
    const rows = buildSearchRows({ hits, loaded, query: 'nata', overlay: new Map([[6, { pinned: true }]]), now: NOW });
    expect(rows.map((r) => [r.id, r.pinned, r.snippet?.length ?? 0])).toEqual([
      [5, false, 1],
      [6, true, 0],
    ]);
  });
  it('falls back to the title filter without an index, and hides deleted chats', () => {
    const loaded = [summary(5, 'Trip', '2026-10-01T09:00:00Z'), summary(6, 'Trip notes', '2026-10-02T09:00:00Z')];
    expect(buildSearchRows({ hits: null, loaded, query: 'trip', overlay: new Map([[6, { deleted: true }]]) }).map((r) => r.id)).toEqual([5]);
  });
});

const ROWS: ChatListItem[] = [
  { id: 1, name: 'Q3 planning', updated_at: '2026-10-07T11:00:00Z', age: '1h', pinned: true },
  { id: 2, name: 'Film club', updated_at: '2026-10-07T10:00:00Z', age: '2h' },
];

describe('Chats rows: pin, rename, delete (issue #6)', () => {
  it('shows Pinned first, says "pinned" and offers Unpin / Pin, Rename and Delete to screen readers', async () => {
    const actions = { onDelete: jest.fn(async () => true), onTogglePin: jest.fn(), onRename: jest.fn() };
    await render(<ConversationList rows={ROWS} now={NOW} onOpen={jest.fn()} actions={actions} empty={<Text>none</Text>} />);
    expect(screen.getByText('PINNED')).toBeTruthy(); // iOS section headers are uppercase
    const pinned = screen.getByRole('button', { name: 'Q3 planning, pinned, 1h' });
    expect(pinned.props.accessibilityActions).toEqual([
      { name: 'pin', label: 'Unpin' },
      { name: 'rename', label: 'Rename' },
      { name: 'delete', label: 'Delete' },
    ]);
    const other = screen.getByRole('button', { name: 'Film club, 2h' });
    expect(other.props.accessibilityActions[0]).toEqual({ name: 'pin', label: 'Pin' });
    await fireEvent(other, 'accessibilityAction', { nativeEvent: { actionName: 'pin' } });
    expect(actions.onTogglePin).toHaveBeenCalledWith(ROWS[1]);
    await fireEvent(other, 'accessibilityAction', { nativeEvent: { actionName: 'rename' } });
    expect(actions.onRename).toHaveBeenCalledWith(ROWS[1]);
    await act(async () => {
      await fireEvent(other, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    });
    expect(actions.onDelete).toHaveBeenCalledWith(ROWS[1]);
  });

  it('a long press opens the same actions as a menu', async () => {
    const actions = { onDelete: jest.fn(async () => true), onTogglePin: jest.fn(), onRename: jest.fn() };
    await render(<ConversationList rows={ROWS} now={NOW} onOpen={jest.fn()} actions={actions} empty={<Text>none</Text>} />);
    await fireEvent(screen.getByRole('button', { name: 'Film club, 2h' }), 'longPress');
    expect(screen.getByRole('button', { name: 'Pin' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Rename' })).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Rename' }));
    expect(actions.onRename).toHaveBeenCalledWith(ROWS[1]);
  });

  it('without contract 1.4 only Delete is offered', async () => {
    await render(<ConversationList rows={ROWS} now={NOW} onOpen={jest.fn()} actions={{ onDelete: jest.fn(async () => true), onTogglePin: null, onRename: null }} empty={<Text>none</Text>} />);
    expect(screen.getByRole('button', { name: 'Film club, 2h' }).props.accessibilityActions).toEqual([{ name: 'delete', label: 'Delete' }]);
  });

  it('search results are one Results section, a message hit with its passage (matched words in bold)', async () => {
    const rows: ChatListItem[] = [
      {
        id: 3,
        name: 'Trip to Lisbon',
        updated_at: '2026-10-01T09:00:00Z',
        age: '6d',
        snippet: [
          { text: '…then ', match: false },
          { text: 'tram', match: true },
          { text: ' 28 up to the castle', match: false },
        ],
      },
    ];
    await render(<ConversationList rows={rows} results now={NOW} onOpen={jest.fn()} actions={null} empty={<Text>none</Text>} />);
    expect(screen.getByText('RESULTS')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Trip to Lisbon, 6d, …then tram 28 up to the castle' })).toBeTruthy();
    expect(screen.getByTestId('chat-snippet')).toBeTruthy();
  });
});

describe('usePendingDelete (undo, then sync)', () => {
  beforeEach(() => jest.useFakeTimers());
  afterEach(() => jest.useRealTimers());

  it('commits after the undo window; Undo keeps the row', async () => {
    const commit = jest.fn();
    const { result } = await renderHook(() => usePendingDelete<number, string>('A', commit));
    await act(async () => result.current.start(1));
    expect(result.current.pending).toBe(1);
    await act(async () => result.current.undo());
    await act(async () => jest.advanceTimersByTime(UNDO_WINDOW_MS * 2));
    expect(commit).not.toHaveBeenCalled();
    await act(async () => result.current.start(2));
    await act(async () => jest.advanceTimersByTime(UNDO_WINDOW_MS));
    expect(commit).toHaveBeenCalledWith(2, 'A');
    expect(result.current.pending).toBeNull();
  });

  it('a second delete commits the first at once; leaving the screen commits the waiting one', async () => {
    const commit = jest.fn();
    const { result, unmount } = await renderHook(() => usePendingDelete<number, string>('A', commit));
    await act(async () => result.current.start(1));
    await act(async () => result.current.start(2));
    expect(commit).toHaveBeenCalledWith(1, 'A');
    await act(async () => unmount());
    expect(commit).toHaveBeenLastCalledWith(2, 'A');
  });

  it('commits against the project the row came from: switching project sends the waiting delete there at once', async () => {
    // Conversation ids are per project: (B, 42) is another chat than (A, 42).
    const commit = jest.fn();
    const { result, rerender } = await renderHook(({ project }: { project: string }) => usePendingDelete<number, string>(project, commit), {
      initialProps: { project: 'A' },
    });
    await act(async () => result.current.start(42));
    expect(result.current.pendingScope).toBe('A');
    await act(async () => rerender({ project: 'B' }));
    expect(commit).toHaveBeenCalledTimes(1);
    expect(commit).toHaveBeenCalledWith(42, 'A');
    expect(result.current.pending).toBeNull();
    await act(async () => jest.advanceTimersByTime(UNDO_WINDOW_MS * 2));
    expect(commit).toHaveBeenCalledTimes(1);
  });

  it('keeps a committed row hidden until its commit settled (no flash back when the window ends)', async () => {
    // The queued path resolves only once the list overlay shows the delete.
    let settle: () => void = () => {};
    const commit = jest.fn(() => new Promise<void>((resolve) => (settle = resolve)));
    const { result } = await renderHook(() => usePendingDelete<number, string>('A', commit));
    await act(async () => result.current.start(5));
    expect(result.current.waiting).toEqual([{ row: 5, scope: 'A' }]);
    await act(async () => jest.advanceTimersByTime(UNDO_WINDOW_MS));
    expect(commit).toHaveBeenCalledWith(5, 'A');
    expect(result.current.pending).toBeNull();
    expect(result.current.waiting).toEqual([{ row: 5, scope: 'A' }]);
    await act(async () => settle());
    expect(result.current.waiting).toEqual([]);
  });

  it('a second delete keeps the first hidden while it is being committed', async () => {
    const settles: (() => void)[] = [];
    const commit = jest.fn(() => new Promise<void>((resolve) => settles.push(resolve)));
    const { result, unmount } = await renderHook(() => usePendingDelete<number, string>('A', commit));
    await act(async () => result.current.start(1));
    await act(async () => result.current.start(2));
    expect(result.current.waiting.map((w) => w.row).sort()).toEqual([1, 2]);
    await act(async () => settles[0]());
    expect(result.current.waiting.map((w) => w.row)).toEqual([2]);
    await act(async () => unmount());
  });

  it('a timer that fires after a re-render still commits with the scope captured at start', async () => {
    const commit = jest.fn();
    const scopes = { current: 'A' };
    const { result, rerender } = await renderHook(() => usePendingDelete<number, string>(scopes.current, commit, () => true));
    await act(async () => result.current.start(7));
    scopes.current = 'B';
    await act(async () => rerender({}));
    await act(async () => jest.advanceTimersByTime(UNDO_WINDOW_MS));
    expect(commit).toHaveBeenCalledWith(7, 'A');
  });
});

describe('UndoToast', () => {
  it('announces every delete, a second one while the toast is up included, with what Undo now restores', async () => {
    const announce = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    const a = { id: 1, name: 'Q3 planning' };
    const b = { id: 2, name: 'Film club' };
    const toast = (row: { id: number; name: string }) => <UndoToast text="Chat deleted" subject={row} subjectName={row.name} onUndo={jest.fn()} />;
    const view = await render(toast(a));
    await view.rerender(toast(b));
    expect(announce.mock.calls.map((call) => call[0])).toEqual([
      'Chat deleted: “Q3 planning”. Undo is available.',
      'Chat deleted: “Film club”. Undo is available.',
    ]);
    announce.mockRestore();
  });
});

describe('canSaveName', () => {
  it('needs a non-blank name that differs from the current one', () => {
    expect(canSaveName('  ', 'A')).toBe(false);
    expect(canSaveName(' A ', 'A')).toBe(false);
    expect(canSaveName('B', 'A')).toBe(true);
  });
});

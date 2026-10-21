import { act, fireEvent, render, screen } from '@testing-library/react-native';
import { Text } from 'react-native';

import { ConversationList } from '@/features/chats/ConversationList';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const NOW = Date.parse('2026-10-07T12:00:00Z');
const ROW = { id: 5, name: 'Launch plan', updated_at: '2026-10-07T11:00:00Z', age: '1h' };

describe('Chats rows (F5)', () => {
  it('reads as busy to screen readers while a Delete started from the row action is pending', async () => {
    let finish: (removed: boolean) => void = () => {};
    const onDelete = jest.fn(() => new Promise<boolean>((resolve) => (finish = resolve)));
    await render(<ConversationList rows={[ROW]} now={NOW} onOpen={jest.fn()} onDelete={onDelete} empty={<Text>none</Text>} />);
    const row = () => screen.getByRole('button', { name: 'Launch plan, 1h' });
    expect(row().props.accessibilityState.busy).toBeUndefined();
    expect(row().props.accessibilityActions).toEqual([{ name: 'delete', label: 'Delete' }]);
    await fireEvent(row(), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(row().props.accessibilityState).toMatchObject({ busy: true });
    // A second Delete while one is pending is ignored (and the row said so).
    await fireEvent(row(), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    expect(onDelete).toHaveBeenCalledTimes(1);
    await act(async () => finish(false));
    expect(row().props.accessibilityState.busy).toBeUndefined();
  });
});

describe('Chats rows: activity status (client contract 1.12)', () => {
  const at = '2026-10-07T11:00:00Z';
  const rows = [
    { id: 1, name: 'Running one', updated_at: at, age: '1h', activity: 'running' as const },
    { id: 2, name: 'Waiting one', updated_at: at, age: '1h', activity: 'needs_you' as const, needsYou: true },
    { id: 3, name: 'Unread one', updated_at: at, age: '1h', activity: 'unread' as const, unread: true },
    { id: 4, name: 'Read one', updated_at: at, age: '1h', activity: 'done' as const },
    { id: 5, name: 'Old unread', updated_at: at, age: '1h', unread: true },
    { id: 6, name: 'Pinned waiting', updated_at: at, age: '1h', pinned: true, activity: 'needs_you' as const, needsYou: true },
  ];
  const weight = (name: string) => {
    const flat = [screen.getByText(name).props.style].flat(Infinity).filter(Boolean) as { fontWeight?: string }[];
    return flat.reduce<string | undefined>((w, s) => s.fontWeight ?? w, undefined);
  };

  it('draws the glyph, says it, and bolds an unread title', async () => {
    await render(<ConversationList rows={rows} now={NOW} onOpen={jest.fn()} empty={<Text>none</Text>} />);
    for (const kind of ['running', 'needs_you', 'unread', 'done']) expect(screen.getAllByTestId(`activity-${kind}`, { includeHiddenElements: true }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Running one, Running, 1h' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Waiting one, Needs your input, 1h' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Unread one, Finished, unread, 1h' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Read one, Finished, 1h' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Old unread, Unread, 1h' })).toBeTruthy();
    expect(weight('Unread one')).not.toBe(weight('Read one'));
    expect(weight('Old unread')).toBe(weight('Unread one'));
  });

  it('heads each section with its "N needs you" count, and nothing at zero', async () => {
    await render(<ConversationList rows={rows} now={NOW} onOpen={jest.fn()} empty={<Text>none</Text>} />);
    expect(screen.getAllByTestId('section-needs-you').map((node) => node.props.children)).toEqual(['1 needs you', '1 needs you']);
    await render(<ConversationList rows={[rows[0], rows[3]]} now={NOW} onOpen={jest.fn()} empty={<Text>none</Text>} />);
    expect(screen.queryByTestId('section-needs-you')).toBeNull();
  });

  it('lets a busy row (a fork) keep its spinner over the glyph', async () => {
    await render(<ConversationList rows={[rows[1]]} now={NOW} onOpen={jest.fn()} busyId={2} busyText="Forking…" empty={<Text>none</Text>} />);
    expect(screen.getByTestId('chat-row-busy')).toBeTruthy();
    expect(screen.queryByTestId('activity-needs_you', { includeHiddenElements: true })).toBeNull();
  });
});

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

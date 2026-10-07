import { fireEvent, render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { notificationsPage } from '@/api/__fixtures__/settings13';
import NotificationsScreen from '@/app/(app)/notifications';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

let mockRows: unknown[] = [];
const mockDismiss = jest.fn(async (id: number) => {
  mockRows = mockRows.filter((r) => (r as { id: number }).id !== id);
  return true;
});
jest.mock('@/api/queries/notifications', () => {
  const actual = jest.requireActual('@/api/queries/notifications');
  return {
    ...actual,
    useNotifications: () => ({ isLoading: false, error: null, data: mockRows, refetch: jest.fn(), isRefetching: false }),
    useInvalidateNotifications: () => jest.fn(),
    useDismissNotification: () => mockDismiss,
  };
});
let mockContract = '1.3';
jest.mock('@/state/workspaceInfo', () => ({
  ...jest.requireActual('@/state/workspaceInfo'),
  useWorkspaceInfo: () => ({ origin: 'https://ai.example.com', discovery: { client_contract: mockContract } }),
}));

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: 'aaaaaaaaaaaa', projectId: 90503 });
  mockRows = [...notificationsPage.rows];
  mockContract = '1.3';
  jest.clearAllMocks();
});

it('filters the inbox by type', async () => {
  await render(<NotificationsScreen />);
  expect(screen.getAllByRole('tab').map((t) => t.props.accessibilityLabel)).toEqual([
    'All, 3',
    'Agent finished, 1',
    'Mentions, 1',
    'Moderation approved, 1',
  ]);
  await fireEvent.press(screen.getByTestId('filter-chat_user_mentioned'));
  expect(screen.getByText('Bob Zefir mentioned you')).toBeTruthy();
  expect(screen.queryByTestId('notification-10')).toBeNull();
  expect(screen.getByTestId('filter-chat_user_mentioned').props.accessibilityState).toMatchObject({ selected: true });
});

it('dismisses a row through its screen-reader action (the same path as the swipe)', async () => {
  const view = await render(<NotificationsScreen />);
  const row = screen.getByLabelText('Agent finished');
  expect(row.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Dismiss' }]);
  await fireEvent(row, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
  expect(mockDismiss).toHaveBeenCalledWith(10);
  await view.rerender(<NotificationsScreen />);
  expect(screen.queryByTestId('notification-10')).toBeNull();
});

it('the Dismiss action sits on the element a screen reader focuses, linked or not', async () => {
  await render(<NotificationsScreen />);
  // VoiceOver/TalkBack offer only the focused accessibility element's custom actions, so the
  // action must be on the accessible row itself, not on a non-accessible wrapper around it.
  const plain = screen.getByLabelText('Agent finished');
  expect(plain.props.accessible).toBe(true);
  expect(plain.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Dismiss' }]);
  const link = screen.getByRole('link');
  expect(link.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Dismiss' }]);
  await fireEvent(link, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
  expect(mockDismiss).toHaveBeenCalledWith(9);
});

it('a server before client mockContract 1.3 offers no mockDismiss', async () => {
  mockContract = '1.2';
  await render(<NotificationsScreen />);
  expect(screen.getByLabelText('Agent finished').props.accessibilityActions).toBeUndefined();
  expect(screen.getByRole('link').props.accessibilityActions).toBeUndefined();
});

it('falls back to All when the last row of the chosen type is dismissed', async () => {
  const view = await render(<NotificationsScreen />);
  await fireEvent.press(screen.getByTestId('filter-chat_user_mentioned'));
  await fireEvent(screen.getByRole('link'), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
  await view.rerender(<NotificationsScreen />);
  expect(screen.getByTestId('notification-10')).toBeTruthy();
  expect(screen.getByTestId('filter-all').props.accessibilityState).toMatchObject({ selected: true });
});

it('the chip bar keeps its own height instead of growing into the list (a horizontal ScrollView grows by default)', async () => {
  await render(<NotificationsScreen />);
  const bar = screen.getByTestId('notification-filters');
  expect(StyleSheet.flatten(bar.props.style)).toMatchObject({ flexGrow: 0 });
});

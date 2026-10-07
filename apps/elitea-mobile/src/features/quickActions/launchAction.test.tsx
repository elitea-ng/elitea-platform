import { render } from '@testing-library/react-native';
import { router } from 'expo-router';

import { __resetInitialQuickActionForTests, useQuickActions } from '@/features/quickActions/useQuickActions';
import { __resetSessionForTests, setSession } from '@/state/session';

/**
 * A quick action that COLD-starts the app. Under the scene life cycle (SDK 58,
 * `ExpoAppSceneDelegate`) UIKit no longer puts the shortcut item in the app
 * delegate's launch options, so expo-quick-actions' `initial` constant is
 * null; the item arrives as a "perform" event before any JS listener exists.
 * The patched native module keeps it for `takeInitial()` (patches/expo-quick-actions+6.0.2.patch).
 */

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-router', () => ({ router: { navigate: jest.fn() } }));
jest.mock('@/api/queries/conversations', () => ({ useConversations: () => ({ data: undefined }) }));
jest.mock('@/api/queries/notifications', () => ({ useUnseenCount: () => ({ data: 0 }) }));

let mockPending: unknown = null;
const mockTakeInitial = jest.fn(() => {
  const action = mockPending;
  mockPending = null;
  return action;
});
jest.mock('@/features/quickActions/nativeQuickActions', () => ({ quickActionsNative: () => ({ takeInitial: mockTakeInitial }) }));

function Probe() {
  useQuickActions();
  return null;
}

beforeEach(() => {
  jest.useFakeTimers();
  jest.clearAllMocks();
  __resetSessionForTests();
  __resetInitialQuickActionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: 'aaaaaaaaaaaa', projectId: 90501 });
});

afterEach(() => {
  jest.useRealTimers();
});

it('opens the screen of the quick action that cold-started the app (kept by the native module)', async () => {
  mockPending = { id: 'notifications', title: 'Notifications', params: { href: '/notifications', workspace: 'aaaaaaaaaaaa' } };
  await render(<Probe />);
  jest.runOnlyPendingTimers();
  expect(mockTakeInitial).toHaveBeenCalledTimes(1);
  expect(router.navigate).toHaveBeenCalledWith('/notifications');
});

it('a kept action built for another workspace opens nothing', async () => {
  mockPending = { id: 'notifications', title: 'Notifications', params: { href: '/notifications', workspace: 'bbbbbbbbbbbb' } };
  await render(<Probe />);
  jest.runOnlyPendingTimers();
  expect(router.navigate).not.toHaveBeenCalled();
});

it('runs the launch action once per process, not on every remount', async () => {
  mockPending = { id: 'new-chat', title: 'New chat', params: { href: '/new', workspace: 'aaaaaaaaaaaa' } };
  const first = await render(<Probe />);
  jest.runOnlyPendingTimers();
  await first.unmount();
  mockPending = { id: 'new-chat', title: 'New chat', params: { href: '/new', workspace: 'aaaaaaaaaaaa' } };
  await render(<Probe />);
  jest.runOnlyPendingTimers();
  expect(router.navigate).toHaveBeenCalledTimes(1);
});

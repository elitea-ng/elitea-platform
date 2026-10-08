import { act, renderHook } from '@testing-library/react-native';
import { Alert } from 'react-native';

import { identity } from '@/config/identity';
import { useIncomingConnectLink } from '@/linking/useIncomingConnectLink';
import { DEVICE_KEYS, SECURE_KEYS } from '@/security/secureStorage';
import { __resetRegistryForTests } from '@/security/workspaceRegistry';
import { __resetSessionForTests, setSession } from '@/state/session';
import { __resetWorkspaceListForTests, refreshWorkspaceList } from '@/state/workspaceList';
import { registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const mockUrl: { current: string | null } = { current: null };
jest.mock('expo-linking', () => ({ useLinkingURL: () => mockUrl.current }));
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const A = 'aaaaaaaaaaaa';
const B = 'bbbbbbbbbbbb';
const ORIGIN_A = 'https://a.example.com';
const ORIGIN_B = 'https://b.example.com';

beforeEach(() => {
  __resetSessionForTests();
  __resetRegistryForTests();
  __resetWorkspaceListForTests();
  fake.values.clear();
  jest.spyOn(Alert, 'alert').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

// F5: at cold start the workspace list is still loading when the launch URL
// is classified; a link for a connected workspace must not read as "add".
it('waits for the workspace list before classifying the launch link', async () => {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, A), ORIGIN_A);
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, B), ORIGIN_B);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A, B], A));
  setSession({ hydrated: true, serverOrigin: ORIGIN_A, workspaceId: A });
  mockUrl.current = `${identity.connectSchemes[0]}://connect?server=${encodeURIComponent(ORIGIN_B)}`;

  const { unmount } = await renderHook(() => useIncomingConnectLink(true));
  expect(Alert.alert).not.toHaveBeenCalled();

  await act(async () => {
    await refreshWorkspaceList();
  });

  expect(Alert.alert).toHaveBeenCalledTimes(1);
  expect((Alert.alert as jest.Mock).mock.calls[0][0]).toBe('Switch workspace?');
  await unmount();
});

// #46: with no workspace active the connect screen reads the link's `server`
// param itself. `useLinkingURL` keeps returning that URL, so once the user
// picked a workspace there (another one than the link's) the same URL must
// not come back as "Add workspace?".
it('a link the connect screen already read is not offered again once a workspace is active', async () => {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, A), ORIGIN_A);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A], A));
  await refreshWorkspaceList();
  setSession({ hydrated: true, serverOrigin: null, workspaceId: null });
  mockUrl.current = `${identity.connectSchemes[0]}://connect?server=${encodeURIComponent('https://alias.example.com')}`;

  const { rerender, unmount } = await renderHook(() => useIncomingConnectLink(true));
  expect(Alert.alert).not.toHaveBeenCalled();

  await act(async () => {
    setSession({ serverOrigin: ORIGIN_A, workspaceId: A });
  });
  await rerender({});

  expect(Alert.alert).not.toHaveBeenCalled();
  await unmount();
});

it('a new connect link opened after that is still offered', async () => {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, A), ORIGIN_A);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A], A));
  await refreshWorkspaceList();
  setSession({ hydrated: true, serverOrigin: ORIGIN_A, workspaceId: A });
  mockUrl.current = `${identity.connectSchemes[0]}://connect?server=${encodeURIComponent('https://c.example.com')}`;

  const { unmount } = await renderHook(() => useIncomingConnectLink(true));

  expect(Alert.alert).toHaveBeenCalledTimes(1);
  expect((Alert.alert as jest.Mock).mock.calls[0][0]).toBe('Add workspace?');
  await unmount();
});

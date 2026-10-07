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

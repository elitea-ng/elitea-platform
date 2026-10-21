import { renderHook } from '@testing-library/react-native';
import * as AppIntents from 'expo-app-intents';

import { useDiscardIntentsWhileSignedOut } from '@/features/intents/discardIntentsWhileSignedOut';

const mock = AppIntents as unknown as {
  pending: { id: string; name: string; params: Record<string, unknown>; createdAt: number }[];
  addAppIntentListener: jest.Mock;
  clearPendingInvocationsAsync: jest.Mock;
};

beforeEach(() => {
  mock.pending.splice(0);
  jest.clearAllMocks();
});

it('while signed out, a queued question is cleared at once and each new one as it arrives', async () => {
  mock.pending.push({ id: 'a', name: 'askAgent', params: { text: 'a question for nobody' }, createdAt: Date.now() });
  const { unmount } = await renderHook(() => useDiscardIntentsWhileSignedOut(true));
  await Promise.resolve();
  expect(mock.pending).toHaveLength(0);

  const listener = mock.addAppIntentListener.mock.calls[0]![0] as () => void;
  mock.pending.push({ id: 'b', name: 'askAgent', params: { text: 'again' }, createdAt: Date.now() });
  listener();
  await Promise.resolve();
  expect(mock.pending).toHaveLength(0);
  await unmount();
});

it('signed in: leaves the queue to the signed-in handler', async () => {
  mock.pending.push({ id: 'a', name: 'openNotifications', params: {}, createdAt: Date.now() });
  await renderHook(() => useDiscardIntentsWhileSignedOut(false));
  await Promise.resolve();
  expect(mock.pending).toHaveLength(1);
  expect(mock.addAppIntentListener).not.toHaveBeenCalled();
});

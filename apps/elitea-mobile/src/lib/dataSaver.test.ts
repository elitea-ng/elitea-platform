import { act, renderHook } from '@testing-library/react-native';

import { avatarMotion } from '@elitea/client-core/conversation';

import { isDataSaverOn, setDataSaverLoader, useDataSaver, type DataSaverNative } from '@/lib/dataSaver';

function fakeNative(initial: boolean) {
  const handlers: ((change: { lowData?: unknown }) => void)[] = [];
  const remove = jest.fn();
  const native: DataSaverNative = {
    isLowData: jest.fn(() => initial),
    addListener: jest.fn((_event, listener) => {
      handlers.push(listener);
      return { remove };
    }),
  };
  return { native, remove, push: (change: { lowData?: unknown }) => handlers.forEach((h) => h(change)) };
}

afterEach(() => setDataSaverLoader(() => null));

describe('data saver', () => {
  it('is off when the native module is absent', () => {
    setDataSaverLoader(() => null);
    expect(isDataSaverOn()).toBe(false);
  });

  it('reads the system state and follows its changes', async () => {
    const fake = fakeNative(true);
    setDataSaverLoader((name) => (name === 'DataSaver' ? fake.native : null));
    const { result, unmount } = await renderHook(() => useDataSaver());
    expect(result.current).toBe(true);
    await act(async () => fake.push({ lowData: false }));
    expect(result.current).toBe(false);
    await unmount();
    expect(fake.remove).toHaveBeenCalled();
  });

  it('makes the avatar show its still', async () => {
    const fake = fakeNative(true);
    setDataSaverLoader(() => fake.native);
    const { result } = await renderHook(() => useDataSaver());
    const states = { 4: { state: 'thinking' as const, since: 1_000 } };
    const ask = (dataSaver: boolean) => avatarMotion({ participantId: 4, newest: true }, states, 1_000, { reducedMotion: false, dataSaver });
    expect(ask(result.current)).toBe('still');
    expect(ask(false)).toBe('thinking');
  });
});

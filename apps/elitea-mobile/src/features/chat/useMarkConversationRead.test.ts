import { act, renderHook } from '@testing-library/react-native';
import { QueryClient } from '@tanstack/react-query';
import type { AppStateStatus } from 'react-native';

import { runMarkConversationRead } from '@/api/queries/markRead';
import { MARK_READ_SETTLE_MS, useMarkConversationRead, type MarkReadEnvironment } from '@/features/chat/useMarkConversationRead';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/api/queries/markRead', () => ({ runMarkConversationRead: jest.fn() }));

const run = runMarkConversationRead as jest.MockedFunction<typeof runMarkConversationRead>;

function environment() {
  const listeners = new Set<(state: AppStateStatus) => void>();
  const env = {
    state: 'active' as AppStateStatus,
    online: true,
    emit(state: AppStateStatus) {
      env.state = state;
      for (const listener of [...listeners]) listener(state);
    },
    value: {} as MarkReadEnvironment,
  };
  env.value = {
    appActivity: {
      get currentState() {
        return env.state;
      },
      addEventListener: (_type, listener) => {
        listeners.add(listener);
        return { remove: () => listeners.delete(listener) };
      },
    },
    network: { isOnline: () => env.online },
    deps: { queryClient: new QueryClient(), getDb: () => null },
  };
  return env;
}

type Props = { revision?: unknown; enabled?: boolean; conversationId?: number | null };

beforeEach(() => {
  jest.useFakeTimers();
  run.mockReset();
  run.mockResolvedValue(true);
});
afterEach(() => jest.useRealTimers());

const settle = () => act(async () => void jest.advanceTimersByTime(MARK_READ_SETTLE_MS));

describe('useMarkConversationRead (client contract 1.12)', () => {
  it('marks the open conversation read once it settles, again on a new answer and on return to the foreground', async () => {
    const env = environment();
    const { rerender } = await renderHook((props: Props) =>
      useMarkConversationRead({ projectId: 7, conversationId: props.conversationId ?? 41, revision: props.revision, enabled: props.enabled ?? true, environment: env.value }),
      { initialProps: { revision: '3:false' } },
    );
    expect(run).not.toHaveBeenCalled();
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenLastCalledWith(7, 41, env.value.deps);

    // A burst of answers sends one read.
    await rerender({ revision: '4:false' });
    await rerender({ revision: '4:true' });
    await settle();
    expect(run).toHaveBeenCalledTimes(2);

    await act(async () => env.emit('background'));
    await act(async () => env.emit('active'));
    await settle();
    expect(run).toHaveBeenCalledTimes(3);
  });

  it('sends nothing when off (a server before 1.12, the offline copy, covered), offline or in the background', async () => {
    const env = environment();
    const { rerender } = await renderHook((props: Props) =>
      useMarkConversationRead({ projectId: 7, conversationId: props.conversationId === undefined ? 41 : props.conversationId, revision: props.revision, enabled: props.enabled ?? true, environment: env.value }),
      { initialProps: { enabled: false } as Props },
    );
    await settle();
    await rerender({ enabled: true, conversationId: null });
    await settle();
    env.online = false;
    await rerender({ enabled: true, revision: 1 });
    await settle();
    env.online = true;
    env.state = 'background';
    await rerender({ enabled: true, revision: 2 });
    await settle();
    expect(run).not.toHaveBeenCalled();
  });

  it('runs one read at a time and sends the one asked for meanwhile after it', async () => {
    const env = environment();
    let finish: () => void = () => {};
    run.mockImplementationOnce(() => new Promise<boolean>((resolve) => (finish = () => resolve(true))));
    const { rerender } = await renderHook((props: Props) => useMarkConversationRead({ projectId: 7, conversationId: 41, revision: props.revision, enabled: true, environment: env.value }), {
      initialProps: { revision: 1 } as Props,
    });
    await settle();
    await rerender({ revision: 2 });
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
    await act(async () => finish());
    expect(run).toHaveBeenCalledTimes(2);
  });
});

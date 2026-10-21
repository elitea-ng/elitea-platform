import { act, renderHook } from '@testing-library/react-native';
import { QueryClient } from '@tanstack/react-query';
import type { AppStateStatus } from 'react-native';

import { runMarkConversationRead } from '@/api/queries/markRead';
import {
  MARK_READ_RETRY_LIMIT,
  MARK_READ_RETRY_MS,
  MARK_READ_SETTLE_MS,
  readRevision,
  useMarkConversationRead,
  type MarkReadEnvironment,
} from '@/features/chat/useMarkConversationRead';

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
  run.mockResolvedValue({ kind: 'read', unread: false });
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
    run.mockImplementationOnce(() => new Promise((resolve) => (finish = () => resolve({ kind: 'read', unread: false }))));
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

  it('reads again shortly while still on screen when the workspace answered unread, a bounded number of times', async () => {
    const env = environment();
    run.mockResolvedValue({ kind: 'read', unread: true });
    const { rerender } = await renderHook((props: Props) => useMarkConversationRead({ projectId: 7, conversationId: 41, revision: 1, enabled: props.enabled ?? true, environment: env.value }), {
      initialProps: {} as Props,
    });
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
    for (let i = 0; i < MARK_READ_RETRY_LIMIT + 2; i += 1) await act(async () => void jest.advanceTimersByTime(MARK_READ_RETRY_MS));
    expect(run).toHaveBeenCalledTimes(1 + MARK_READ_RETRY_LIMIT);

    // Covered (or closed) before the retry is due: none is sent.
    run.mockClear();
    await act(async () => env.emit('active'));
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
    await rerender({ enabled: false });
    await act(async () => void jest.advanceTimersByTime(MARK_READ_RETRY_MS * 3));
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('never sends the read again for a conversation the workspace refused for good', async () => {
    const env = environment();
    run.mockResolvedValue({ kind: 'refused' });
    const { rerender } = await renderHook((props: Props) => useMarkConversationRead({ projectId: 7, conversationId: 41, revision: props.revision, enabled: true, environment: env.value }), {
      initialProps: { revision: 1 } as Props,
    });
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
    await rerender({ revision: 2 });
    await settle();
    await act(async () => env.emit('active'));
    await settle();
    expect(run).toHaveBeenCalledTimes(1);
  });
});

describe('readRevision', () => {
  const message = (key: string, createdAt: number, extra: { isStreaming?: boolean; error?: string | null } = {}) => ({
    key,
    createdAt,
    isStreaming: extra.isStreaming ?? false,
    error: extra.error ?? null,
  });

  it('changes when a new answer replaces another without changing the count', () => {
    const before = readRevision([message('a', 1), message('b', 2)], true);
    expect(readRevision([message('b', 2), message('c', 3)], true)).not.toBe(before);
    expect(readRevision([message('a', 1), message('b', 2)], true)).toBe(before);
  });

  it('changes when the newest answer finishes streaming, fails, or the live turn settles', () => {
    const streaming = readRevision([message('a', 1, { isStreaming: true })], false);
    expect(readRevision([message('a', 1)], false)).not.toBe(streaming);
    expect(readRevision([message('a', 1, { error: 'boom' })], false)).not.toBe(readRevision([message('a', 1)], false));
    expect(readRevision([message('a', 1)], true)).not.toBe(readRevision([message('a', 1)], false));
    expect(readRevision([], false)).not.toBe(readRevision([], true));
  });
});

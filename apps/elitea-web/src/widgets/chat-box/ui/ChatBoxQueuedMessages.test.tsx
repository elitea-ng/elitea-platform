/**
 * A17 — the queue MODEL (where a send goes, when the queue drains) and the
 * "Waiting messages" strip.
 *
 * ## Why these assertions and not the obvious ones
 *
 * The queue's whole behaviour is a pair of transitions nobody can see from a
 * component render: "a run opened, so this send is queued" and "the run
 * settled, so exactly one queued message goes out". Both are driven here by
 * flipping `isStreaming` on a `renderHook` rerender — the same signal `ChatBox`
 * derives from the transport — and by a `send` stub that resolves when the test
 * says so, so the SECOND delivery can be proved to wait for the first turn
 * rather than racing it.
 *
 * The delivery path is asserted as the ORDINARY send (`sendQuestion`), because
 * the alternative — a second transport, a direct POST — is exactly what would
 * make a queued message skip admission, budget and guardrails.
 */
import { ThemeProvider } from '@mui/material/styles';
import { act, render, renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import type { ChatMessage } from '@/features/chat-messages';

import { DRAFT_CONVERSATION_KEY, useQueuedMessagesStore } from '../model/queuedMessages.store';
import {
  ChatBoxQueuedMessages,
  chatBoxQueueKey,
  useChatBoxQueuedMessages,
  type ChatBoxQueuedMessagesModel,
  type QueuedSendOutcome,
} from './ChatBoxQueuedMessages';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const KEY = 'conv-A17';

/** A `send` stub that resolves immediately — `Promise.resolve`, not `async`, so the lint gate does not see an `await`-less async function. */
const resolving = (outcome: QueuedSendOutcome = {}) => () => Promise.resolve(outcome);

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return <ThemeProvider theme={theme}>{children}</ThemeProvider>;
}

interface HarnessProps {
  readonly isStreaming: boolean;
  readonly onSendNow: (question: string) => void;
  readonly send: (params: { readonly question: string }) => Promise<QueuedSendOutcome>;
  readonly onConversationCreated?: (conversation: { readonly id?: string | number; readonly uuid?: string }) => void;
  readonly conversationKey?: string;
}

function renderModel(initial: HarnessProps) {
  return renderHook(
    (props: HarnessProps) =>
      useChatBoxQueuedMessages({
        conversationKey: props.conversationKey ?? KEY,
        isStreaming: props.isStreaming,
        onSendNow: props.onSendNow,
        send: props.send,
        onConversationCreated: props.onConversationCreated,
      }),
    { initialProps: initial },
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  useQueuedMessagesStore.resetForTests();
});

describe('where a send goes', () => {
  it('sends straight away when nothing is running', async () => {
    const onSendNow = vi.fn();
    const send = vi.fn(resolving());
    const { result } = renderModel({ isStreaming: false, onSendNow, send });

    act(() => { result.current.onSend('an ordinary question'); });

    expect(onSendNow).toHaveBeenCalledWith('an ordinary question');
    expect(result.current.items).toHaveLength(0);
    // Nothing is delivered from the queue, because nothing entered it.
    await waitFor(() => { expect(send).not.toHaveBeenCalled(); });
  });

  it('queues while a turn is open, in the order typed (ELITEA-2864)', () => {
    const onSendNow = vi.fn();
    const { result } = renderModel({ isStreaming: true, onSendNow, send: vi.fn(resolving()) });

    act(() => { result.current.onSend('make it short'); });
    act(() => { result.current.onSend('make it easy'); });
    act(() => { result.current.onSend('add examples'); });

    // The composer's send NEVER reaches the wire mid-run: `onSendNow` is the
    // path that would start a second turn, and the whole point is that it does
    // not run while one is open.
    expect(onSendNow).not.toHaveBeenCalled();
    expect(result.current.items.map((row) => row.text)).toEqual(['make it short', 'make it easy', 'add examples']);
  });

  it('refuses a blank message either way (ELITEA-2873)', () => {
    const onSendNow = vi.fn();
    const { result } = renderModel({ isStreaming: true, onSendNow, send: vi.fn(resolving()) });
    act(() => { result.current.onSend('   \t '); });
    expect(result.current.items).toHaveLength(0);
    expect(onSendNow).not.toHaveBeenCalled();
  });
});

describe('when the queue drains', () => {
  it('delivers exactly one message per settle, in FIFO order (ELITEA-2865)', async () => {
    const resolvers: ((outcome: QueuedSendOutcome) => void)[] = [];
    const send = vi.fn(
      (_params: { readonly question: string }) =>
        new Promise<QueuedSendOutcome>((resolve) => { resolvers.push(resolve); }),
    );
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send };
    const { result, rerender } = renderModel(base);

    act(() => { result.current.onSend('first'); });
    act(() => { result.current.onSend('second'); });
    expect(send).not.toHaveBeenCalled();

    // The run ends. ONE message goes out.
    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(1); });
    expect(send).toHaveBeenLastCalledWith({ question: 'first' });
    // And the second one is still WAITING: a queue that fired everything at
    // once would put two turns on a conversation the server admits one at a
    // time, and the second would be refused, not merely delayed.
    expect(result.current.items.map((row) => row.text)).toEqual(['second']);

    // That delivery's own turn opens…
    act(() => { resolvers[0]?.({ questionId: 'q-first' }); });
    rerender({ ...base, isStreaming: true });
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(1); });

    // …and settles. Now the second goes.
    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(2); });
    expect(send).toHaveBeenLastCalledWith({ question: 'second' });
  });

  it('drains the queue a Stop left behind (ELITEA-2868)', async () => {
    const send = vi.fn(resolving());
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('Change the approach'); });

    // Stop is not a special case here, and that is the design: it settles the
    // run exactly as a completed turn does, so the queue delivers rather than
    // being discarded.
    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(send).toHaveBeenCalledWith({ question: 'Change the approach' }); });
    expect(useQueuedMessagesStore.getState().queues[KEY]).toBeUndefined();
  });

  it('keeps moving when a delivery fails outright', async () => {
    const send = vi.fn<(params: { readonly question: string }) => Promise<QueuedSendOutcome>>()
      .mockRejectedValueOnce(new Error('no transport'))
      .mockResolvedValue({});
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('one'); });
    act(() => { result.current.onSend('two'); });

    rerender({ ...base, isStreaming: false });
    // A failed send never opens a stream, so `isStreaming` never rises to
    // re-arm the pass. The remainder must still go out — a `useRef` guard here
    // would strand it, which is why the guard is state.
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(2); });
    expect(result.current.items).toHaveLength(0);
  });

  it('never delivers a message the reader removed', async () => {
    const send = vi.fn(resolving());
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('keep me'); });
    act(() => { result.current.onSend('drop me'); });
    act(() => { result.current.onRemove(result.current.items[1]?.id ?? ''); });

    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(1); });
    expect(send).toHaveBeenCalledWith({ question: 'keep me' });
  });

  it('announces a conversation a delivery committed', async () => {
    const onConversationCreated = vi.fn();
    const send = vi.fn(resolving({ createdConversation: { id: 42, uuid: 'u-42' } }));
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send, onConversationCreated };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('queued before the row existed'); });
    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(onConversationCreated).toHaveBeenCalledWith({ id: 42, uuid: 'u-42' }); });
  });
});

describe('the "Sent while running" mark (ELITEA-2870)', () => {
  it('stamps the delivered question and nothing else', async () => {
    const send = vi.fn(resolving({ questionId: 'q-1' }));
    const base: HarnessProps = { isStreaming: true, onSendNow: vi.fn(), send };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('interjected'); });
    rerender({ ...base, isStreaming: false });
    await waitFor(() => { expect(send).toHaveBeenCalled(); });

    const history: readonly ChatMessage[] = [
      { id: 'q-0', role: 'user', name: 'A', content: 'typed normally', createdAt: '' },
      { id: 'q-1', role: 'user', name: 'A', content: 'interjected', createdAt: '' },
    ];
    await waitFor(() => {
      expect(result.current.decorate(history)[1]?.interjected).toBe(true);
    });
    expect(result.current.decorate(history)[0]?.interjected).toBeUndefined();
  });

  it('returns the SAME array when nothing is marked', () => {
    const { result } = renderModel({ isStreaming: false, onSendNow: vi.fn(), send: vi.fn(resolving()) });
    const history: readonly ChatMessage[] = [{ id: 'x', role: 'user', name: 'A', content: 'c', createdAt: '' }];
    // Referential identity matters: `ChatMessageList` is memoised on it, and a
    // fresh array every render would re-render the whole transcript per frame
    // of a streaming answer.
    expect(result.current.decorate(history)).toBe(history);
  });
});

describe('chatBoxQueueKey', () => {
  it('prefers the uuid, falls back to the id, then to the draft sentinel', () => {
    expect(chatBoxQueueKey('uuid-1', 7)).toBe('uuid-1');
    expect(chatBoxQueueKey(undefined, 7)).toBe('7');
    expect(chatBoxQueueKey(undefined, undefined)).toBe(DRAFT_CONVERSATION_KEY);
  });

  it('adopts the draft queue when the conversation arrives', async () => {
    const send = vi.fn(resolving());
    const base: HarnessProps = {
      isStreaming: true, onSendNow: vi.fn(), send, conversationKey: DRAFT_CONVERSATION_KEY,
    };
    const { result, rerender } = renderModel(base);
    act(() => { result.current.onSend('typed on a brand-new chat'); });

    rerender({ ...base, conversationKey: 'conv-new' });
    await waitFor(() => {
      expect(result.current.items.map((row) => row.text)).toEqual(['typed on a brand-new chat']);
    });
  });
});

/* ── the strip ───────────────────────────────────────────────────────────── */

function stripModel(items: readonly { id: string; text: string; queuedAt: number }[], onRemove = vi.fn()): ChatBoxQueuedMessagesModel {
  return { items, onSend: vi.fn(), onRemove, decorate: (messages) => messages };
}

describe('ChatBoxQueuedMessages', () => {
  it('renders nothing at all while the queue is empty (ELITEA-2869)', () => {
    render(<ChatBoxQueuedMessages queue={stripModel([])} />, { wrapper });
    expect(screen.queryByTestId('chat-queued-messages')).toBeNull();
  });

  it('counts the waiting messages and shows each with a Queued status', () => {
    render(
      <ChatBoxQueuedMessages
        queue={stripModel([
          { id: '1', text: 'First queued message', queuedAt: 1 },
          { id: '2', text: 'Second queued message', queuedAt: 2 },
        ])}
      />,
      { wrapper },
    );
    expect(screen.getByTestId('chat-queued-count')).toHaveTextContent('Waiting messages · 2');
    const rows = screen.getAllByTestId('chat-queued-item');
    // Chronological, first at top — the order they will be delivered in.
    expect(rows[0]).toHaveTextContent('First queued message');
    expect(rows[1]).toHaveTextContent('Second queued message');
    expect(screen.getAllByTestId('chat-queued-status')).toHaveLength(2);
  });

  /*
   * The i18next pluralization trap, pinned. `{{count}}` in the bundle value is
   * ALSO the option that selects a `_one`/`_other` suffixed key, and this
   * bundle carries neither — so the SINGULAR case is the one that would
   * silently degrade if that fallback ever changed. Both counts are asserted
   * against the real `en.json`, not against the call-site fallback.
   */
  it('counts one waiting message as well as many', () => {
    render(<ChatBoxQueuedMessages queue={stripModel([{ id: '1', text: 'only one', queuedAt: 1 }])} />, { wrapper });
    expect(screen.getByTestId('chat-queued-count')).toHaveTextContent('Waiting messages · 1');
  });

  it('removes the row the reader points at, not the first one', async () => {
    const onRemove = vi.fn();
    render(
      <ChatBoxQueuedMessages
        queue={stripModel([
          { id: 'a', text: 'keep', queuedAt: 1 },
          { id: 'b', text: 'drop', queuedAt: 2 },
        ], onRemove)}
      />,
      { wrapper },
    );
    await userEvent.click(screen.getAllByTestId('chat-queued-remove')[1] as HTMLElement);
    expect(onRemove).toHaveBeenCalledWith('b');
  });
});

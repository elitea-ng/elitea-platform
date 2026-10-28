/**
 * The chat's composer queue: the reader's waiting steering messages, then the A17 ones, and what Send now, Edit and
 * Remove do with each kind. The rows are given their steering messages (`pending`); `useOwnPendingSteering`, which
 * reads them for `ChatBoxComposerQueue`, is tested with its store in `features/chat-messages`.
 */
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createRef } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { SteeringItem } from '@elitea/client-core/stream';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { ComposerQueueRows, type ComposerQueueRowsProps } from './ChatBoxComposerQueue';
import type { ChatBoxQueuedMessagesModel } from './ChatBoxQueuedMessages';

const pending = { items: [] as SteeringItem[] };

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const steer = (id: string, content: string): SteeringItem => ({ id, responseMessageId: 'r1', authorId: 1, content, state: 'pending', revision: 1, queueOrder: 1, messageId: null });

function setup(overrides: Partial<ComposerQueueRowsProps> = {}) {
  const queue: ChatBoxQueuedMessagesModel = {
    items: [{ id: 'q1', text: 'after the turn', queuedAt: 1 }],
    onSend: vi.fn(),
    enqueue: vi.fn(() => 'q-new'),
    onRemove: vi.fn(),
    onPromote: vi.fn(),
    decorate: (messages) => messages,
  };
  const steering = { remove: vi.fn<(item: SteeringItem) => Promise<boolean>>().mockResolvedValue(true) };
  const onInterrupt = vi.fn();
  const handle = { value: 'typed', getInputContent: () => handle.value, setValue: (value: string) => { handle.value = value; } };
  const input = createRef<typeof handle>() as { current: typeof handle | null };
  input.current = handle;
  render(
    <ThemeProvider theme={theme}>
      <ComposerQueueRows pending={pending.items} queue={queue} steering={steering} isStreaming onInterrupt={onInterrupt} input={input} {...overrides} />
    </ThemeProvider>,
  );
  return { queue, steering, onInterrupt, handle };
}

const row = (testId: string): HTMLElement => screen.getByTestId(testId);
const click = async (testId: string, name: string): Promise<void> => userEvent.click(within(row(testId)).getByRole('button', { name }));

beforeEach(() => {
  pending.items = [steer('s1', 'also check the logs')];
});

describe('ChatBoxComposerQueue', () => {
  it('lists the waiting steering messages first, then the A17 ones, under one caption', () => {
    setup();
    const list = screen.getByTestId('chat-queued-messages');
    expect(within(list).getAllByText('Queued')).toHaveLength(1);
    expect(row('chat-steering-item')).toHaveTextContent('also check the logs');
    expect(row('chat-steering-item')).toHaveAttribute('data-state', 'pending');
    expect(row('chat-queued-item')).toHaveTextContent('after the turn');
    expect(row('chat-steering-item').compareDocumentPosition(row('chat-queued-item')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('shows nothing when both queues are empty', () => {
    setup({ pending: [], queue: { items: [], onSend: vi.fn(), enqueue: vi.fn(), onRemove: vi.fn(), onPromote: vi.fn(), decorate: (m) => m } });
    expect(screen.queryByTestId('chat-queued-messages')).toBeNull();
  });

  it('Send now while no turn runs only reorders the queue', async () => {
    const { queue, onInterrupt } = setup({ isStreaming: false, pending: [] });
    await click('chat-queued-item', 'Send now');
    expect(queue.onPromote).toHaveBeenCalledWith('q1');
    expect(onInterrupt).not.toHaveBeenCalled();
  });

  it('Send now on a steering message: off the turn, to the head of the A17 queue, and the turn stops', async () => {
    const { queue, steering, onInterrupt } = setup();
    await click('chat-steering-item', 'Send now');
    await waitFor(() => expect(onInterrupt).toHaveBeenCalledOnce());
    expect(steering.remove).toHaveBeenCalledWith(pending.items[0]);
    expect(queue.enqueue).toHaveBeenCalledWith('also check the logs');
    expect(queue.onPromote).toHaveBeenCalledWith('q-new');
  });

  it('Send now on a steering message the turn already took does nothing more', async () => {
    const { queue, steering, onInterrupt } = setup();
    steering.remove.mockResolvedValue(false);
    await click('chat-steering-item', 'Send now');
    await waitFor(() => expect(steering.remove).toHaveBeenCalled());
    expect(queue.enqueue).not.toHaveBeenCalled();
    expect(onInterrupt).not.toHaveBeenCalled();
  });

  it('Send now on an A17 message moves it to the head and stops the turn', async () => {
    const { queue, onInterrupt } = setup();
    await click('chat-queued-item', 'Send now');
    expect(queue.onPromote).toHaveBeenCalledWith('q1');
    expect(onInterrupt).toHaveBeenCalledOnce();
    expect(queue.onRemove).not.toHaveBeenCalled();
  });

  it('Edit takes the message off and puts it back before what is typed', async () => {
    const { queue, steering, handle } = setup();
    await click('chat-steering-item', 'Edit');
    await waitFor(() => expect(handle.value).toBe('also check the logs\n\ntyped'));
    expect(steering.remove).toHaveBeenCalledOnce();
    await click('chat-queued-item', 'Edit');
    expect(queue.onRemove).toHaveBeenCalledWith('q1');
    expect(handle.value).toBe('after the turn\n\nalso check the logs\n\ntyped');
  });

  it('Remove takes the message off, and the turn runs on', async () => {
    const { queue, steering, onInterrupt, handle } = setup();
    await click('chat-steering-item', 'Remove');
    await click('chat-queued-item', 'Remove');
    await waitFor(() => expect(steering.remove).toHaveBeenCalledOnce());
    expect(queue.onRemove).toHaveBeenCalledWith('q1');
    expect(onInterrupt).not.toHaveBeenCalled();
    expect(handle.value).toBe('typed');
  });
});

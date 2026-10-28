/**
 * The chat's queue at the top of the composer (`QueuedMessages`, the same rows as a Local work thread's), oldest
 * first: the reader's steering messages that wait for the running turn (`useOwnPendingSteering`, contract 1.27),
 * then the A17 messages that wait for the turn to settle (`useChatBoxQueuedMessages`).
 *
 * - Send now: the message goes first, as the next message, and the turn stops. A steering message comes off the
 *   turn and into the head of the A17 queue; an A17 message moves to its head. The turn stops WITHOUT holding its
 *   leftovers (`onInterrupt`, not the composer's Stop): the end-of-turn rule hands the other steering messages off
 *   and sends them after it, as "Stop and send" did.
 * - Edit: the message comes off the queue and goes back into the message box, before what is typed there.
 * - Remove: the message comes off the queue.
 *
 * A steering message the turn already injected cannot come off (`remove` resolves `false`): then nothing else
 * happens, and the transcript shows it as delivered.
 */
import type { ReactNode, RefObject } from 'react';

import type { SteeringItem } from '@elitea/client-core/stream';

import { useOwnPendingSteering, type ChatSteeringModel } from '@/features/chat-messages';
import { QueuedMessages, type QueuedMessage } from '@/shared/ui/queued-messages';

import type { ChatBoxQueuedMessagesModel } from './ChatBoxQueuedMessages';

/** The part of the composer's handle Edit uses. */
interface DraftInput {
  getInputContent(): string;
  setValue(value: string): void;
}

/** The rows and what they do, given the reader's pending steering messages (`pending`). */
export interface ComposerQueueRowsProps {
  readonly pending: readonly SteeringItem[];
  readonly queue: ChatBoxQueuedMessagesModel;
  readonly steering: Pick<ChatSteeringModel, 'remove'>;
  readonly isStreaming: boolean;
  /** Stops the running turn and lets the end-of-turn rule send its leftovers. */
  readonly onInterrupt: () => void;
  readonly input: RefObject<DraftInput | null>;
}

export interface ChatBoxComposerQueueProps extends Omit<ComposerQueueRowsProps, 'pending'> {
  readonly conversationKey: string;
}

/** Reads the reader's pending steering messages of the conversation; the rows are `ComposerQueueRows`. */
export function ChatBoxComposerQueue({ conversationKey, ...props }: ChatBoxComposerQueueProps): ReactNode {
  const pending = useOwnPendingSteering(conversationKey, props.isStreaming);
  return <ComposerQueueRows pending={pending} {...props} />;
}

const STEER = 'steer:';
const QUEUED = 'queued:';

/** The text before what the input holds, a blank line between. */
function putBack(input: RefObject<DraftInput | null>, text: string): void {
  const handle = input.current;
  if (handle === null) return;
  handle.setValue([text, handle.getInputContent()].filter((part) => part.trim() !== '').join('\n\n'));
}

export function ComposerQueueRows({ pending, queue, steering, isStreaming, onInterrupt, input }: ComposerQueueRowsProps): ReactNode {
  const items: QueuedMessage[] = [
    ...pending.map((item) => ({ id: `${STEER}${item.id}`, text: item.content, testId: 'chat-steering-item', state: item.state })),
    ...queue.items.map((item) => ({ id: `${QUEUED}${item.id}`, text: item.text, testId: 'chat-queued-item' })),
  ];
  const interrupt = (): void => {
    if (isStreaming) onInterrupt();
  };
  /** Takes the row off its queue, then hands its text on (not when a steering message could not come off). */
  const take = (id: string, then: (text: string) => void): void => {
    const steer = pending.find((item) => `${STEER}${item.id}` === id);
    if (steer !== undefined) {
      void steering.remove(steer).then((removed) => removed && then(steer.content));
      return;
    }
    const row = queue.items.find((item) => `${QUEUED}${item.id}` === id);
    if (row === undefined) return;
    queue.onRemove(row.id);
    then(row.text);
  };
  const sendNow = (id: string): void => {
    const row = queue.items.find((item) => `${QUEUED}${item.id}` === id);
    if (row !== undefined) {
      queue.onPromote(row.id);
      interrupt();
      return;
    }
    take(id, (text) => {
      const added = queue.enqueue(text);
      if (added !== undefined) queue.onPromote(added);
      interrupt();
    });
  };
  return (
    <QueuedMessages
      testId="chat-queued-messages"
      items={items}
      onSendNow={sendNow}
      onEdit={(id) => take(id, (text) => putBack(input, text))}
      onRemove={(id) => take(id, () => undefined)}
    />
  );
}

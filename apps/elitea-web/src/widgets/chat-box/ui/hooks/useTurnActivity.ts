/**
 * Tells the host when a turn starts and ends in the conversation on screen
 * (`ChatBoxProps.conversation.onTurn`): `sent` when the composer's turn opens
 * (a send, a regenerate, a reattached run), `settled` when it closes. The chat
 * page uses it to move the conversation to Today in the conversation lists.
 *
 * A change of conversation is not a turn: the edge is read only while the
 * same conversation stays on screen.
 */
import { useEffect, useRef } from 'react';

type ChatBoxTurnPhase = 'sent' | 'settled';
export type ChatBoxTurnSink = (phase: ChatBoxTurnPhase, conversationId: string) => void;

export function useTurnActivity(isStreaming: boolean, rawId: string | number | undefined, onTurn: ChatBoxTurnSink | undefined): void {
  const conversationId = rawId === undefined ? undefined : String(rawId);
  const previous = useRef<{ readonly id: string | undefined; readonly streaming: boolean }>({ id: conversationId, streaming: isStreaming });
  const sink = useRef(onTurn);
  sink.current = onTurn;
  useEffect(() => {
    const before = previous.current;
    previous.current = { id: conversationId, streaming: isStreaming };
    if (conversationId === undefined || conversationId === '' || before.id !== conversationId || before.streaming === isStreaming) return;
    sink.current?.(isStreaming ? 'sent' : 'settled', conversationId);
  }, [conversationId, isStreaming]);
}

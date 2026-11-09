/**
 * The participant an unaddressed message goes to, derived ONCE per render
 * (ADR-0050 D7c, issue 1625).
 *
 * The send (`useChatBoxSend`: the start body and the stream reducer's
 * identity), the attachments gate (`useChatBoxParticipant`) and "To:"
 * (`ChatBoxFloorTarget`) all need it. Each used to re-derive it from the
 * active participant, the roster and a hand-threaded sticky id, so dropping
 * the sticky id from one consumer let the send and the gate disagree on the
 * target with no test failing. They now take the resolved participant.
 */
import { useMemo } from 'react';

import { resolveTargetParticipant, stickyAgentParticipantId } from './useChatBoxSend.helpers';

export function useResolvedTarget(
  activeParticipant: unknown,
  participants: readonly unknown[] | undefined,
  messages: readonly { readonly participantId?: string | undefined }[] | undefined,
): unknown {
  // With several agents and none picked, the agent the history last addressed.
  const stickyParticipantId = useMemo(() => stickyAgentParticipantId(messages, participants), [messages, participants]);
  return useMemo(
    () => resolveTargetParticipant(activeParticipant, participants, stickyParticipantId),
    [activeParticipant, participants, stickyParticipantId],
  );
}

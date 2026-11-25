/**
 * The participant an unaddressed message goes to, derived ONCE per render
 * (ADR-0050 D7c, issue 1625).
 *
 * The send (`useChatBoxSend`: the start body and the stream reducer's
 * identity), the attachments gate (`useChatBoxParticipant`) and "To:"
 * (`ChatBoxFloorTarget`) all need it. Each used to re-derive it from the
 * active participant, the roster and a hand-threaded sticky id, so dropping
 * the sticky id from one consumer let the send and the gate disagree on the
 * target with no test failing. They now take the resolved participant, typed
 * with its id read once (`ResolvedTarget`, issue 1859).
 *
 * With nothing picked and a lead on the conversation's floor (`meta.floor`),
 * the lead: Main answers an unaddressed message from the lead, and "To:" names
 * it, so the send names it too (a fresh team conversation, issue 3049).
 */
import { useMemo } from 'react';

import { resolveTargetParticipant, stickyAgentParticipantId } from './useChatBoxSend.helpers';
import { leadParticipantOf, resolvedTarget, type ResolvedTarget } from './useChatBoxSend.floor';

export type { ResolvedTarget } from './useChatBoxSend.floor';

/** The participant row an unaddressed message goes to: a pick, else the floor's lead, else the only or sticky agent. */
export function targetParticipant(
  activeParticipant: unknown,
  participants: readonly unknown[] | undefined,
  meta: unknown,
  stickyParticipantId?: string,
): unknown {
  const picked = activeParticipant !== undefined && activeParticipant !== null;
  const lead = picked ? undefined : leadParticipantOf(meta, participants);
  return lead ?? resolveTargetParticipant(activeParticipant, participants, stickyParticipantId);
}

export function useResolvedTarget(
  activeParticipant: unknown,
  participants: readonly unknown[] | undefined,
  messages: readonly { readonly participantId?: string | undefined }[] | undefined,
  meta?: unknown,
): ResolvedTarget {
  // With several agents and none picked, the agent the history last addressed. A pick wins, so the
  // history (a new array on every streamed frame) is not scanned then.
  const picked = activeParticipant !== undefined && activeParticipant !== null;
  const stickyParticipantId = useMemo(() => (picked ? undefined : stickyAgentParticipantId(messages, participants)), [picked, messages, participants]);
  return useMemo(
    () => resolvedTarget(targetParticipant(activeParticipant, participants, meta, stickyParticipantId)),
    [activeParticipant, participants, meta, stickyParticipantId],
  );
}

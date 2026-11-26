/**
 * The live reply's activity label (ADR-0061 decision 4c, #3069): what the answering agent does
 * now, read from the stream's `agent_state` frames (state and `activity`, client contract 1.38
 * plus #3068). Text only, so it shows under reduced motion and data saver as well; the avatar
 * itself follows `avatarMotion`'s rules.
 */
import { AGENT_STATE } from '@elitea/client-core/stream';
import { activityLabel, shownState, type AvatarStates, type ParticipantName } from '@elitea/client-core/conversation';

import { TEAM_COPY } from '@/features/localWork/teamCopy';

/** The label of `participantId`'s state, or null when no frame named it or it is idle. */
export function liveActivityLabel(states: AvatarStates, participantId: number | null, now: number, names: ParticipantName): string | null {
  if (participantId === null) return null;
  const entry = states[participantId];
  if (entry === undefined) return null;
  const state = shownState(entry, now);
  if (state === AGENT_STATE.Idle) return null;
  return activityLabel(state, entry.activity, names, TEAM_COPY.activity);
}

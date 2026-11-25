/**
 * Issue 3049: a fresh team conversation whose floor has a lead showed
 * "To: <lead> (lead)" but sent the message as a model chat with no
 * participant, and Main answered `agent_target_required`. With nothing
 * picked, the send's target is the lead, so the start is the lead's agent
 * turn and "To:" names the agent the send names.
 */
import { describe, expect, it } from 'vitest';

import { conversationApi } from '@/entities/conversation';

import { floorTarget } from '../ChatBoxFloorTarget';
import { leadParticipantOf, resolvedTarget } from './useChatBoxSend.floor';
import { resolveStartContract } from './useChatBoxSend.helpers';
import { targetParticipant } from './useResolvedTarget';

const coordinator = { id: '41', entity_name: 'application', entity_meta: { id: 7, name: 'Dev Coordinator' } };
const reviewer = { id: '42', entity_name: 'application', entity_meta: { id: 8, name: 'Reviewer' } };
const model = { id: '43', entity_name: 'dummy', entity_meta: {} };
const person = { id: '44', entity_name: 'user', entity_meta: { id: 1 } };
const participants = [coordinator, reviewer, person, model];
const leadMeta = { floor: { policy: 'lead', lead_participant_id: 41 } };
const agents = [
  { participantId: 41, name: 'Dev Coordinator' },
  { participantId: 42, name: 'Reviewer' },
];

describe('the send target under a lead', () => {
  it('is the lead when nothing is picked, as "To:" says', () => {
    const target = resolvedTarget(targetParticipant(undefined, participants, leadMeta));
    expect(target.participant).toBe(coordinator);
    expect(target.participantId).toBe(41);
    expect(resolveStartContract(target.participant)).toBe(conversationApi.contracts.application);
    expect(floorTarget({ agents, meta: leadMeta, target })?.agent.participantId).toBe(target.participantId);
  });

  it('is the lead over the sticky agent, which Main would overrule', () => {
    expect(targetParticipant(null, participants, leadMeta, '42')).toBe(coordinator);
  });

  it('keeps a pick', () => {
    expect(targetParticipant(reviewer, participants, leadMeta)).toBe(reviewer);
    expect(targetParticipant(model, participants, leadMeta)).toBe(model);
  });

  it('is unresolved under `addressed` with nothing picked: "To:" asks to pick and Main refuses', () => {
    const target = resolvedTarget(targetParticipant(undefined, participants, {}));
    expect(target.participant).toBeUndefined();
    expect(floorTarget({ agents, meta: {}, target })).toBeUndefined();
  });

  it('ignores a lead that is no longer an agent of the conversation', () => {
    const gone = { floor: { policy: 'lead', lead_participant_id: 99 } };
    expect(leadParticipantOf(gone, participants)).toBeUndefined();
    expect(leadParticipantOf({ floor: { policy: 'lead', lead_participant_id: 44 } }, participants)).toBeUndefined();
  });
});

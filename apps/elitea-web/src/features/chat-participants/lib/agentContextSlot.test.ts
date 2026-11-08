import { describe, expect, it } from 'vitest';

import { ChatParticipantType } from '../model/constants';
import type { TransformedParticipant } from '../model/types';
import { agentContextOf, agentParticipantId } from './agentContextSlot';

const row = (entity_name: ChatParticipantType, id?: unknown): TransformedParticipant =>
  ({ entity_name, entity_meta: { id: '3' }, entity_settings: {}, ...(id === undefined ? {} : { id }) });

describe('agentContextSlot', () => {
  it('gives a meter to agent rows only, keyed by the participant row id', () => {
    expect(agentParticipantId(row(ChatParticipantType.Applications, 12))).toBe(12);
    expect(agentParticipantId(row(ChatParticipantType.Toolkits, 12))).toBeUndefined();
    expect(agentParticipantId(row(ChatParticipantType.Applications))).toBeUndefined();
    expect(agentParticipantId(row(ChatParticipantType.Applications, '12'))).toBeUndefined();
  });

  it('renders the slot for the agent and nothing without one', () => {
    const render = ({ participantId }: { participantId: number }) => `meter ${participantId}`;
    expect(agentContextOf(render, row(ChatParticipantType.Applications, 12))).toEqual({ contextSlot: 'meter 12' });
    expect(agentContextOf(render, row(ChatParticipantType.Toolkits, 12))).toEqual({});
    expect(agentContextOf(undefined, row(ChatParticipantType.Applications, 12))).toEqual({});
  });
});

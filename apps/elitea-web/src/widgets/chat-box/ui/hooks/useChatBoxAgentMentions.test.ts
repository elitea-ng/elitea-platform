/**
 * The composer's agent addressing (ADR-0050 7c, issue 1612): which agents
 * "@" and "#" offer, how an "@" row says it is an agent, which mentions are
 * highlighted, and who "To:" says answers an unaddressed message.
 */
import { describe, expect, it } from 'vitest';

import { matchingAgents } from '../ChatBoxPopups';
import { floorTarget } from '../ChatBoxFloorTarget';
import { agentCandidates, agentMentionRanges, agentOfCandidate, conversationAgents, isMultiAgent } from './useChatBoxAgentMentions';

const PARTICIPANTS = [
  { id: 1, entity_name: 'user', meta: { user_name: 'Reader' } },
  { id: 12, entity_name: 'application', entity_meta: { name: 'release-bot' }, persona: { display_name: 'Mira' } },
  { id: '13', entity_name: 'application', entity_meta: { name: 'CI Doctor' } },
  { id: 14, entity_name: 'toolkit', entity_meta: { name: 'github' } },
];

describe('conversationAgents', () => {
  it('lists the application participants by persona name, else agent name', () => {
    expect(conversationAgents(PARTICIPANTS)).toEqual([
      { participantId: 12, name: 'Mira' },
      { participantId: 13, name: 'CI Doctor' },
    ]);
    expect(isMultiAgent(conversationAgents(PARTICIPANTS))).toBe(true);
    expect(isMultiAgent(conversationAgents(PARTICIPANTS.slice(0, 2)))).toBe(false);
  });

  it('marks agent rows of the "@" list so a pick is not taken for a person', () => {
    const [row] = agentCandidates(conversationAgents(PARTICIPANTS));
    expect(agentOfCandidate(row)).toEqual({ participantId: 12, name: 'Mira' });
    expect(agentOfCandidate({ id: '7', name: 'Reader' })).toBeUndefined();
  });

  it('filters the "#" list by the typed query', () => {
    expect(matchingAgents(conversationAgents(PARTICIPANTS), '#ci').map((agent) => agent.name)).toEqual(['CI Doctor']);
    expect(matchingAgents(conversationAgents(PARTICIPANTS), '#zzz')).toEqual([]);
  });
});

describe('agentMentionRanges', () => {
  it('highlights whole-word mentions of the chosen agents only', () => {
    const chosen = [{ participantId: 13, name: 'CI Doctor' }];
    expect(agentMentionRanges('ask #CI Doctor and @CI Doctors', chosen)).toEqual([{ start: 4, end: 14 }]);
  });
});

describe('floorTarget', () => {
  const agents = conversationAgents(PARTICIPANTS);
  const input = { agents, activeParticipant: undefined, participants: PARTICIPANTS, stickyParticipantId: undefined };

  it('names the lead when the floor has one', () => {
    const meta = { floor: { policy: 'lead', lead_participant_id: 13 } };
    expect(floorTarget({ ...input, meta, activeParticipant: PARTICIPANTS[1] })).toEqual({ agent: { participantId: 13, name: 'CI Doctor' }, isLead: true });
  });

  it('otherwise names the selected or sticky agent', () => {
    expect(floorTarget({ ...input, meta: {}, activeParticipant: PARTICIPANTS[1] })?.agent.name).toBe('Mira');
    expect(floorTarget({ ...input, meta: {}, stickyParticipantId: '13' })?.agent.name).toBe('CI Doctor');
  });

  it('names no one when nothing decides it, or with a single agent', () => {
    expect(floorTarget({ ...input, meta: {} })).toBeUndefined();
    expect(floorTarget({ ...input, agents: agents.slice(0, 1), meta: {} })).toBeUndefined();
  });
});

describe('queuedSend', () => {
  it('addresses a queued message from its own text, whatever the composer holds now', async () => {
    const { renderHook } = await import('@testing-library/react');
    const { useChatBoxMentions } = await import('./useChatBoxMentions');
    const sent: unknown[] = [];
    const sendQuestion = (params: unknown) => { sent.push(params); return Promise.resolve({ success: true }); };
    const state = { onInputChange: () => undefined, combinedHighlightRanges: [], skill: {}, keyDown: {} } as never;
    const { result } = renderHook(() => useChatBoxMentions({ state, onChangeParticipant: undefined, participants: PARTICIPANTS, sendQuestion }));
    await result.current.queuedSend({ question: 'please #CI Doctor', attachments: [] });
    await result.current.queuedSend({ question: 'no one', attachments: [] });
    expect(sent).toEqual([
      { question: 'please #CI Doctor', attachments: [], addressedTo: [13] },
      { question: 'no one', attachments: [], addressedTo: [] },
    ]);
  });
});

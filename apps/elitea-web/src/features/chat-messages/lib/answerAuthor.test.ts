import { describe, expect, it } from 'vitest';

import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message/lib/wire';

import { answerAuthor, participantIconUrl } from './answerAuthor';
import { convertMessagesToChatHistory } from './convertMessagesToChatHistory';

const participants: MessageParticipantWire[] = [
  { id: '1', entity_name: 'user', kind: 'human', meta: { user_name: 'Ann' } },
  {
    id: '2',
    entity_name: 'application',
    kind: 'agent',
    entity_meta: { id: 41, name: 'release-bot' },
    entity_settings: { icon_meta: { url: '/api/v2/artifacts/mira.png', name: 'mira.png' } },
    persona: { display_name: 'Mira', role: 'Release manager' },
  },
  { id: '3', entity_name: 'application', kind: 'agent', entity_meta: { id: 42, name: 'CI Doctor' } },
];

function group(fields: Partial<MessageGroupWire>): MessageGroupWire {
  return { id: 10, uuid: 'g-10', content: 'hi', created_at: '2026-11-06T00:00:00Z', reply_to_id: 9, ...fields };
}

describe('answerAuthor', () => {
  it('reads the participant, the persona and the icon', () => {
    expect(answerAuthor(group({ author_participant_id: 2, author_kind: 'agent', addressed_to: [] }), participants)).toEqual({
      authorParticipantId: 2,
      messageClass: 'agent_to_humans',
      personaName: 'Mira',
      authorRole: 'Release manager',
      authorIconUrl: '/api/v2/artifacts/mira.png',
    });
  });

  it('classes agent-to-agent talk from visibility or the roster', () => {
    expect(answerAuthor(group({ author_participant_id: 2, author_kind: 'agent', visibility: 'side' }), participants).messageClass).toBe('agent_to_agent');
    expect(answerAuthor(group({ author_participant_id: '2', author_kind: 'agent', addressed_to: [3] }), participants).messageClass).toBe('agent_to_agent');
    expect(answerAuthor(group({ author_participant_id: 2, author_kind: 'agent', addressed_to: [1] }), participants).messageClass).toBe('agent_to_humans');
  });

  it('a row from before contract 1.38 is an answer to people with no persona', () => {
    expect(answerAuthor(group({ author_participant_id: 3 }), participants)).toEqual({ authorParticipantId: 3, messageClass: 'agent_to_humans' });
    expect(answerAuthor(group({}), participants)).toEqual({ messageClass: 'agent_to_humans' });
  });
});

describe('participantIconUrl', () => {
  it('takes a same-origin path only', () => {
    const icon = (url: unknown): MessageParticipantWire => ({ id: '9', entity_settings: { icon_meta: { url } } });
    expect(participantIconUrl(icon('/a.png'))).toBe('/a.png');
    for (const bad of ['https://cdn.example/a.png', '//evil.example/a.png', '/\\evil.example/a.png', 'javascript:alert(1)', 'http://x/a.png', '', 7]) expect(participantIconUrl(icon(bad))).toBeUndefined();
    expect(participantIconUrl({ id: '9', meta: { icon_meta: { url: '/m.png' } } })).toBe('/m.png');
  });
});

describe('convertMessagesToChatHistory', () => {
  it('names an answer by the persona and carries its avatar fields', () => {
    const [answer] = convertMessagesToChatHistory([group({ author_participant_id: 2, author_kind: 'agent', cause: 'heartbeat' })], participants);
    expect(answer).toMatchObject({
      authorName: 'Mira',
      authorParticipantId: 2,
      authorRole: 'Release manager',
      authorIconUrl: '/api/v2/artifacts/mira.png',
      messageClass: 'heartbeat',
    });
  });

  it('keeps the agent name when the agent has no persona', () => {
    const [answer] = convertMessagesToChatHistory([group({ author_participant_id: 3 })], participants);
    expect(answer?.authorName).toBe('CI Doctor');
    expect(answer?.authorRole).toBeUndefined();
  });
});

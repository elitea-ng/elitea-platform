/**
 * ADR-0050 decision 7c on the web send path (issue 1612): the agents a
 * message addresses reach the start body as `addressed_to`, only when it
 * addresses someone, and the turn goes to an agent the person addressed.
 */
import { describe, expect, it } from 'vitest';

import { addressedTo } from '@elitea/client-core/conversation';
import { ChatSendRequest } from '@/shared/api/generated/model/chatSendRequest.zod';

import { buildDefaultMessagePayload } from './useChatBoxHandlers.helpers';
import { addressedField, addressedIdsOf, NO_TARGET, resolvedTarget, targetForAddressed } from './useChatBoxSend.floor';
import { buildChatStreamContext, buildStartBody } from './useChatBoxSend.helpers';

const base = {
  conversationUuid: '3f8a3d4e-2b1c-4c5d-8e9f-0a1b2c3d4e5f',
  projectId: '1',
  llmSettings: undefined,
  modelName: undefined,
  isApplicationTurn: true,
  participantId: 12,
};

const chosen = [
  { participantId: 12, name: 'Mira' },
  { participantId: 13, name: 'CI Doctor' },
  { participantId: 14, name: 'Scribe' },
];

describe('addressed_to on the start body', () => {
  it('carries the addressed agents of the text, in pick order', () => {
    const text = '@Mira and #CI Doctor, please check the release';
    const payload = buildDefaultMessagePayload({ question: text, questionId: 'q-1', participant: undefined, addressedTo: addressedTo(text, chosen) });
    const body = buildStartBody({ ...base, payload });
    expect(body?.['addressed_to']).toEqual([12, 13]);
  });

  it('leaves the field out when the message addresses no one', () => {
    const text = 'no mention left: Mira was deleted';
    const payload = buildDefaultMessagePayload({ question: text, questionId: 'q-1', participant: undefined, addressedTo: addressedTo(text, chosen) });
    expect(payload).not.toHaveProperty('addressedTo');
    expect(buildStartBody({ ...base, payload })).not.toHaveProperty('addressed_to');
  });

  it('builds a body the generated ChatSendRequest schema accepts', () => {
    const payload = { question: 'hi @Mira @Scribe', question_id: '7c9e6679-7425-40de-944b-e07fc1f90ae7', addressedTo: [12, 14] };
    const body = buildStartBody({ ...base, payload });
    const parsed = ChatSendRequest.safeParse(body);
    expect(parsed.success, JSON.stringify(parsed.error?.issues)).toBe(true);
  });

  it('drops ids that cannot name a participant, keeps each once and caps at 16', () => {
    expect(addressedIdsOf([12, '13', 12, 0, -1, 'x', 1.5])).toEqual([12, 13]);
    expect(addressedIdsOf(Array.from({ length: 20 }, (_, i) => i + 1))).toHaveLength(16);
    expect(addressedField({ addressedTo: [] })).toEqual({});
    expect(addressedField({})).toEqual({});
  });
});

describe('targetForAddressed', () => {
  const mira = { id: '12', entity_name: 'application' };
  const doctor = { id: '13', entity_name: 'application' };
  const participants = [mira, doctor];

  const toDoctor = resolvedTarget(doctor);

  it('keeps the composer target when it is one of the addressees', () => {
    expect(targetForAddressed(toDoctor, participants, [12, 13])).toBe(toDoctor);
  });

  it('sends to the first addressee when the target is not addressed', () => {
    expect(targetForAddressed(toDoctor, participants, [12])).toEqual({ participant: mira, participantId: 12 });
    expect(targetForAddressed(NO_TARGET, participants, [13])).toEqual(toDoctor);
  });

  it('keeps the target when nothing is addressed', () => {
    expect(targetForAddressed(toDoctor, participants, [])).toBe(toDoctor);
  });
});

describe('resolvedTarget (issue 1859)', () => {
  it('reads a positive id once, from a number or its decimal string', () => {
    expect(resolvedTarget({ id: '13' }).participantId).toBe(13);
    expect(resolvedTarget({ id: 13 }).participantId).toBe(13);
  });

  it('gives a zero, negative or non-numeric id no participant id, but keeps the row', () => {
    for (const id of [0, '0', -4, 'abc', '', null, undefined, 1.5]) {
      const row = { id, entity_name: 'application' };
      expect(resolvedTarget(row)).toEqual({ participant: row, participantId: undefined });
    }
  });

  it('is NO_TARGET for nothing', () => {
    expect(resolvedTarget(undefined)).toBe(NO_TARGET);
  });
});

describe('the stream identity reads the target id one way (issue 1859)', () => {
  it('attributes the answer to the resolved id, as a string', () => {
    expect(buildChatStreamContext({ target: resolvedTarget({ id: 13 }) }).participantId).toBe('13');
    expect(buildChatStreamContext({ target: resolvedTarget({ id: '13' }) }).participantId).toBe('13');
  });

  it('attributes nothing to a row with no positive id, as the start body and "To:" do', () => {
    expect(buildChatStreamContext({ target: resolvedTarget({ id: 0 }) })).not.toHaveProperty('participantId');
    expect(buildChatStreamContext({ target: resolvedTarget({ id: 'abc' }) })).not.toHaveProperty('participantId');
  });
});

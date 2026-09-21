/**
 * Regression cover for the execution-contract defect in `useChatBoxSend`.
 *
 * DEFECT — the contract was picked from a page flag no caller sets.
 * `startStreamedExecution` chose
 * `isAgentsPage ? contracts.application : contracts.adhoc`, and the sole
 * `<ChatBox>` render (`pages/chat/index.tsx`) passes no `isAgentsPage`. So
 * `agent.execute.application.v1` was unreachable and EVERY turn — including
 * one addressed to an agent participant — went out as
 * `agent.execute.adhoc.v1`, whose resolver joins on
 * `target_participant.entity_name = 'dummy'` (`internal/db/queries/
 * agent_chat.sql`). An agent participant matches no row, so the route answered
 * `422 unsupported_agent_execution`.
 *
 * The one-line ternary swap alone would not have helped: the two contracts are
 * validated differently in `internal/api/v2/agentexecution/route.go`. The
 * application branch requires `ParticipantID > 0` AND `absentJSON(LLMSettings)`
 * — and `buildStartBody` always emitted an `llm_settings` object — so the
 * application contract would have 422'd on its own body shape.
 */
import { describe, expect, it } from 'vitest';

import { conversationApi } from '@/entities/conversation';

import { buildRegenerateBody, buildStartBody, resolveStartContract, resolveTargetParticipant } from './useChatBoxSend.helpers';

const agent = { id: 42, entity_name: 'application' };
const pipeline = { id: 43, entity_name: 'pipeline' };
const model = { id: 7, entity_name: 'llm' };
const dummy = { id: 2, entity_name: 'dummy' };
const toolkit = { id: 25, entity_name: 'toolkit' };

describe('resolveStartContract', () => {
  it('sends an agent turn under the application contract', () => {
    expect(resolveStartContract(agent)).toBe(conversationApi.contracts.application);
    expect(resolveStartContract(pipeline)).toBe(conversationApi.contracts.application);
  });

  it('sends a plain model turn under the ad-hoc contract', () => {
    expect(resolveStartContract(model)).toBe(conversationApi.contracts.adhoc);
    expect(resolveStartContract(undefined)).toBe(conversationApi.contracts.adhoc);
  });
});

describe('resolveTargetParticipant', () => {
  it('prefers an addressable explicit selection', () => {
    expect(resolveTargetParticipant(agent, [dummy])).toBe(agent);
  });

  it('addresses the conversation’s only agent when nothing is selected', () => {
    expect(resolveTargetParticipant(undefined, [model, agent])).toBe(agent);
  });

  it('stays unresolved when the conversation holds more than one agent', () => {
    expect(resolveTargetParticipant(undefined, [agent, pipeline])).toBeUndefined();
  });

  it('routes a stale toolkit or model selection through the ad-hoc model participant', () => {
    expect(resolveTargetParticipant(toolkit, [dummy, toolkit])).toBe(dummy);
    expect(resolveTargetParticipant(model, [dummy, model])).toBe(dummy);
  });
});

const commonBody = {
  conversationUuid: 'conv-uuid-1',
  projectId: '1',
  payload: { question: 'hi', question_id: 'q-1' },
  llmSettings: { temperature: 0.7, steps_limit: 25 },
  modelName: 'gpt-4o',
};

describe('buildStartBody', () => {
  it('omits llm_settings entirely for an application turn', () => {
    const body = buildStartBody({ ...commonBody, isApplicationTurn: true, participantId: 42 });
    // `absentJSON(body.LLMSettings)` — a present key, even an empty object,
    // is answered 422.
    expect(body).toBeDefined();
    expect(Object.hasOwn(body ?? {}, 'llm_settings')).toBe(false);
    expect(body?.['participant_id']).toBe(42);
  });

  it('refuses to build an application body with no addressable participant', () => {
    // `ParticipantID <= 0` is an instant 422, so no body is better than one
    // that cannot pass; the caller falls back to the socket instead.
    expect(buildStartBody({ ...commonBody, isApplicationTurn: true, participantId: undefined })).toBeUndefined();
  });

  it('keeps the llm_settings object and the 0 default for an ad-hoc turn', () => {
    const body = buildStartBody({ ...commonBody, isApplicationTurn: false, participantId: undefined });
    expect(body?.['participant_id']).toBe(0);
    expect(body?.['llm_settings']).toEqual({ temperature: 0.7, model_name: 'gpt-4o', stream: true });
  });

  it('keeps the agent loop bound out of the provider settings', () => {
    const body = buildStartBody({ ...commonBody, isApplicationTurn: false, participantId: undefined });
    const modelSettings = body?.['llm_settings'] as Record<string, unknown> | undefined;
    expect(modelSettings?.['steps_limit']).toBeUndefined();
  });

  it('encodes project_id as a number, which the route decodes into int64', () => {
    const body = buildStartBody({ ...commonBody, isApplicationTurn: false, participantId: 7 });
    expect(body?.['project_id']).toBe(1);
    expect(body?.['participant_id']).toBe(7);
  });

  /*
   * #984: THE @MENTIONS USED TO STOP HERE.
   *
   * The composer resolves an `@` into `isSendingToUser`/`userIds` and puts
   * them on its payload; this builder emitted `user_input` and `attachments`
   * alone, so every mention made through the UI reached the start route as an
   * ordinary message and notified nobody. The server half has parsed
   * `user_ids` at the TOP level of the body since #977.
   */
  it('carries the composer @mentions as top-level numeric user_ids', () => {
    const body = buildStartBody({
      ...commonBody,
      payload: { question: 'hi', question_id: 'q-1', isSendingToUser: true, userIds: ['11', '12'] },
      isApplicationTurn: true,
      participantId: 42,
    });
    // NUMBERS: `parseMentionedUserIDs` unmarshals into []int64 and answers 400
    // for a list of strings, so forwarding the composer's own spelling would
    // have cost the whole turn rather than the mention.
    expect(body?.['user_ids']).toEqual([11, 12]);
    expect(body?.['is_mentioning_everyone']).toBeUndefined();
  });

  it('asks the server to resolve @everyone rather than sending its own list as the answer', () => {
    const body = buildStartBody({
      ...commonBody,
      payload: {
        question: 'hi', question_id: 'q-1',
        isSendingToUser: true, userIds: ['11'], isMentioningEveryone: true,
      },
      isApplicationTurn: true,
      participantId: 42,
    });
    expect(body?.['is_mentioning_everyone']).toBe(true);
  });

  it('drops an id that cannot name a user rather than failing the turn with it', () => {
    const body = buildStartBody({
      ...commonBody,
      payload: { question: 'hi', question_id: 'q-1', isSendingToUser: true, userIds: ['0', 'abc', '11', '11'] },
      isApplicationTurn: true,
      participantId: 42,
    });
    expect(body?.['user_ids']).toEqual([11]);
  });

  it('emits no mention keys for an ordinary message', () => {
    const body = buildStartBody({ ...commonBody, isApplicationTurn: true, participantId: 42 });
    expect(Object.hasOwn(body ?? {}, 'user_ids')).toBe(false);
    expect(Object.hasOwn(body ?? {}, 'is_mentioning_everyone')).toBe(false);
  });
});

describe('buildRegenerateBody', () => {
  const commonRegeneration = {
    conversationUuid: '00000000-0000-4000-8000-000000000001',
    projectId: '1',
    responseMessageId: '00000000-0000-4000-8000-000000000002',
    questionId: '00000000-0000-4000-8000-000000000003',
    question: 'try again',
    llmSettings: { temperature: 0.4 },
    modelName: 'gpt-4o',
  };

  it('builds the current ad-hoc route body with a fresh generation', () => {
    const body = buildRegenerateBody({
      ...commonRegeneration,
      isApplicationTurn: false,
      participantId: undefined,
    });

    expect(body).toMatchObject({
      project_id: 1,
      participant_id: 0,
      conversation_uuid: commonRegeneration.conversationUuid,
      question_id: commonRegeneration.questionId,
      message_id: commonRegeneration.responseMessageId,
      stream_id: commonRegeneration.responseMessageId,
      updated_items: [],
      payload: {
        user_input: 'try again',
        attachments_info: [],
        mcp_tokens: {},
        llm_settings: { temperature: 0.4, model_name: 'gpt-4o', stream: true },
      },
    });
    expect(body?.['regeneration_id']).toEqual(expect.any(String));
  });

  it('omits model settings for an application regeneration', () => {
    const body = buildRegenerateBody({
      ...commonRegeneration,
      isApplicationTurn: true,
      participantId: 42,
    });

    expect(body?.['participant_id']).toBe(42);
    expect(Object.hasOwn((body?.['payload'] as object | undefined) ?? {}, 'llm_settings')).toBe(false);
  });

  /* issue 980: an EDITED question regenerates through this same route. The body
   * used to be refused here (`return undefined`) because the server refused the
   * field; both ends accept it now, and the body must carry the edit AND run
   * from it — `user_input` and `updated_items` describing two different
   * questions would answer one and store the other. */
  it('carries an edited question and runs from its text', () => {
    const updatedItems = [
      { uuid: '00000000-0000-4000-8000-00000000000a', content: 'the rewritten question', item_type: 'text_message' },
    ];
    const body = buildRegenerateBody({
      ...commonRegeneration,
      isApplicationTurn: true,
      participantId: 42,
      updatedItems,
    });

    expect(body).toMatchObject({
      updated_items: updatedItems,
      payload: { user_input: 'the rewritten question' },
    });
  });

  it('runs a retry from the stored question, with no items', () => {
    const body = buildRegenerateBody({
      ...commonRegeneration,
      isApplicationTurn: true,
      participantId: 42,
    });

    expect(body).toMatchObject({ updated_items: [], payload: { user_input: 'try again' } });
  });

  /* A shape this client should not be building — the route admits exactly one
   * `text_message` entry — falls back to the stored question rather than
   * silently sending half an edit. */
  it('ignores an items array it cannot read as one edited question', () => {
    const body = buildRegenerateBody({
      ...commonRegeneration,
      isApplicationTurn: true,
      participantId: 42,
      updatedItems: [{ content: 'edited' }, { content: 'also edited' }],
    });

    expect(body).toMatchObject({ payload: { user_input: 'try again' } });
  });
});

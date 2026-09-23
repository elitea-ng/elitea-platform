import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';

import { server } from '@/test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import {
  addParticipantIntoConversation,
  deleteParticipantFromConversation,
  updateParticipantLlmSettings,
  updateParticipantSettings,
} from './participantApi';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('addParticipantIntoConversation', () => {
  it('POSTs the participants array to elitea_core/participants/prompt_lib/{projectId}/{conversationId} and normalises the response', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/participants/prompt_lib/7/conv-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json([
          { id: 42, entity_name: 'application', entity_meta: { id: 'a1', name: 'Agent One', project_id: '7' } },
        ]);
      }),
    );

    const result = await addParticipantIntoConversation({
      projectId: 7,
      conversationId: 'conv-1',
      participants: [{ entity_name: 'application', entity_meta: { id: 'a1', name: 'Agent One' } }],
    });

    expect(capturedBody).toEqual([{ entity_name: 'application', entity_meta: { id: 'a1', name: 'Agent One' } }]);
    expect(result).toEqual([
      {
        id: '42',
        entityName: 'application',
        entityMeta: { id: 'a1', name: 'Agent One', projectId: '7' },
      },
    ]);
  });

  it('falls back entity_name to "dummy" for an unrecognised wire value', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/participants/prompt_lib/7/conv-1`, () =>
        HttpResponse.json([{ id: 1, entity_name: 'not-a-real-type' }]),
      ),
    );
    const result = await addParticipantIntoConversation({ projectId: 7, conversationId: 'conv-1', participants: [] });
    expect(result[0]?.entityName).toBe('dummy');
  });
});

describe('deleteParticipantFromConversation', () => {
  it('DELETEs elitea_core/participant/prompt_lib/{projectId}/{conversationId}/{id} and resolves void on 204', async () => {
    let called = false;
    server.use(
      http.delete(`${BASE}/elitea_core/participant/prompt_lib/7/conv-1/p-1`, () => {
        called = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const result = await deleteParticipantFromConversation({ projectId: 7, conversationId: 'conv-1', id: 'p-1' });
    expect(called).toBe(true);
    expect(result).toBeUndefined();
  });
});

describe('updateParticipantSettings', () => {
  it('PUTs elitea_core/entity_settings/prompt_lib/{projectId}/{conversationId}/{participantId} and returns the echoed entity_settings', async () => {
    let capturedBody: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/entity_settings/prompt_lib/7/conv-1/p-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ entity_settings: { agent_type: 'pipeline', version_id: 'v2' } });
      }),
    );
    const result = await updateParticipantSettings({
      projectId: 7,
      conversationId: 'conv-1',
      participantId: 'p-1',
      settings: { agent_type: 'pipeline', version_id: 'v2' },
    });
    expect(capturedBody).toEqual({ agent_type: 'pipeline', version_id: 'v2' });
    expect(result).toEqual({ agent_type: 'pipeline', version_id: 'v2' });
  });
});

describe('updateParticipantLlmSettings', () => {
  it('PATCHes elitea_core/entity_settings/prompt_lib/{projectId}/{conversationId} with the one-element [{participant_id, llm_settings}] array BatchUpdateEntitySettings decodes (A14, ELITEA-0386)', async () => {
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/entity_settings/prompt_lib/7/conv-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    const result = await updateParticipantLlmSettings({
      projectId: 7,
      conversationId: 'conv-1',
      participantId: 'p-1',
      llm_settings: { temperature: 0.5 },
    });
    // Array, keyed by `participant_id` — matches `ConversationsRepo
    // .BatchUpdateEntitySettings` reading `s["participant_id"]` (conversations
    // .go:904), not the plain `{llm_settings}` object the old app sent.
    expect(capturedBody).toEqual([{ participant_id: 'p-1', llm_settings: { temperature: 0.5 } }]);
    expect(result).toEqual({ ok: true });
  });

  it('spreads currentEntitySettings first so version_id/variables/icon_meta survive the full-replace repo write', async () => {
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/entity_settings/prompt_lib/7/conv-1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    await updateParticipantLlmSettings({
      projectId: 7,
      conversationId: 'conv-1',
      participantId: 'p-1',
      currentEntitySettings: { version_id: 'v2', variables: [{ name: 'x' }], icon_meta: { url: 'i.png' } },
      llm_settings: { temperature: 0.9 },
    });
    expect(capturedBody).toEqual([
      { participant_id: 'p-1', version_id: 'v2', variables: [{ name: 'x' }], icon_meta: { url: 'i.png' }, llm_settings: { temperature: 0.9 } },
    ]);
  });
});

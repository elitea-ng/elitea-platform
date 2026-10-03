import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { addToolkitConversationParticipant } from './toolkitChatSession';

const PATH = '*/api/v2/elitea_core/participants/prompt_lib/proj-1/42';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('addToolkitConversationParticipant', () => {
  it('POSTs the bare participant array, not a {participants} wrapper', async () => {
    const participants = [
      { entity_name: 'toolkit', entity_meta: { id: 'tk-1', project_id: 'proj-1' }, entity_settings: { toolkit_type: 'github' } },
    ];
    const added = [{ id: 5, entity_name: 'toolkit', entity_meta: { id: 'tk-1', project_id: 'proj-1' } }];
    let body: unknown;
    server.use(
      http.post(PATH, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(added);
      }),
    );

    const result = await addToolkitConversationParticipant({ projectId: 'proj-1', id: 42, participants });

    expect(Array.isArray(body)).toBe(true);
    expect(body).toEqual(participants);
    expect(result.data).toEqual(added);
  });
});

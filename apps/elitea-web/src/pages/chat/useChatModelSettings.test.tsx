import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useChatModelSettings } from './useChatModelSettings';

const BASE = '/api/v2';

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return <QueryClientProvider client={new QueryClient()}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useChatModelSettings', () => {
  it('loads the persisted values and saves model and loop settings to their separate records', async () => {
    let participantBody: unknown;
    let conversationBody: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/entity_settings/prompt_lib/77/5/p-user`, async ({ request }) => {
        participantBody = await request.json();
        return HttpResponse.json({ entity_settings: participantBody });
      }),
      http.put(`${BASE}/elitea_core/conversation/prompt_lib/77/5`, async ({ request }) => {
        conversationBody = await request.json();
        return HttpResponse.json({ id: '5', name: 'Conversation', meta: (conversationBody as { meta?: unknown }).meta });
      }),
    );
    const activeConversation = {
      id: '5',
      meta: { persona: 'qa', steps_limit: 12 },
      participants: [{
        id: 'p-user',
        entity_name: 'user',
        entity_meta: { id: 9 },
        entity_settings: {
          variables: { locale: 'en' },
          llm_settings: { model_name: 'gpt-5.4-mini', model_project_id: '77', temperature: 0.2, max_tokens: 4000 },
        },
      }],
    };
    const { result } = renderHook(
      () => useChatModelSettings({ activeConversation, projectId: '77', userId: '9' }),
      { wrapper },
    );

    expect(result.current.settings).toMatchObject({
      model_name: 'gpt-5.4-mini',
      model_project_id: 77,
      temperature: 0.2,
      max_tokens: 4000,
      steps_limit: 12,
    });

    act(() => {
      result.current.onSetSettings({ temperature: 0.8, max_tokens: 2048, steps_limit: 8 });
    });

    expect(result.current.settings).toMatchObject({ temperature: 0.8, max_tokens: 2048, steps_limit: 8 });
    await waitFor(() => expect(participantBody).toBeDefined());
    await waitFor(() => expect(conversationBody).toBeDefined());
    expect(participantBody).toStrictEqual({
      variables: { locale: 'en' },
      llm_settings: {
        model_name: 'gpt-5.4-mini',
        model_project_id: 77,
        temperature: 0.8,
        max_tokens: 2048,
      },
    });
    expect(conversationBody).toStrictEqual({ meta: { persona: 'qa', steps_limit: 8 } });
  });

  it('uses a new conversation default without attempting persistence before the conversation exists', async () => {
    let writes = 0;
    server.use(
      http.put(`${BASE}/elitea_core/*`, () => {
        writes += 1;
        return HttpResponse.json({});
      }),
    );
    const { result } = renderHook(
      () => useChatModelSettings({ activeConversation: { isNew: true }, projectId: '77', userId: '9' }),
      { wrapper },
    );

    expect(result.current.settings).toMatchObject({
      temperature: 0.6,
      max_tokens: -1,
      steps_limit: 25,
    });
    act(() => {
      result.current.onSetSettings({ max_tokens: 1024, steps_limit: 6 });
    });
    await Promise.resolve();
    expect(result.current.settings).toMatchObject({ max_tokens: 1024, steps_limit: 6 });
    expect(writes).toBe(0);
  });

  it('does not pin a chat with no saved model to the current project (UI-PD-2)', () => {
    // The project default is a model SHARED from project 1. Defaulting
    // model_project_id to the chat's project 77 made the picker look for it in
    // 77, find nothing and show "None".
    const activeConversation = {
      id: '5',
      participants: [{ id: 'p-user', entity_name: 'user', entity_meta: { id: 9 }, entity_settings: {} }],
    };
    const { result } = renderHook(
      () => useChatModelSettings({ activeConversation, projectId: '77', userId: '9' }),
      { wrapper },
    );

    expect(result.current.settings).not.toHaveProperty('model_project_id');
    expect(result.current.settings).not.toHaveProperty('model_name');
  });

  it('keeps a saved shared model with its own project', () => {
    const activeConversation = {
      id: '5',
      participants: [{
        id: 'p-user',
        entity_name: 'user',
        entity_meta: { id: 9 },
        entity_settings: { llm_settings: { model_name: 'gpt-shared', model_project_id: 1 } },
      }],
    };
    const { result } = renderHook(
      () => useChatModelSettings({ activeConversation, projectId: '77', userId: '9' }),
      { wrapper },
    );

    expect(result.current.settings).toMatchObject({ model_name: 'gpt-shared', model_project_id: 1 });
  });
});

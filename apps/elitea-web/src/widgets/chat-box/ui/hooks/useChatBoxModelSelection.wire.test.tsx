import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useChatModelSettings } from '@/pages/chat/useChatModelSettings';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useChatBoxModelSelection } from './useChatBoxModelSelection';

const BASE = '/api/v2';
const FRESH_CONVERSATION = { isNew: true } as const;

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return <QueryClientProvider client={new QueryClient()}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('model selection with the Main catalogue wire shape', () => {
  it('retains the fresh-chat default when the catalogue project identity is numeric', async () => {
    server.use(http.get(`${BASE}/configurations/models/9`, () => HttpResponse.json({
      items: [{ name: 'E2E-MOCK-MODEL', project_id: 9, default: true }],
      default_model_name: 'E2E-MOCK-MODEL',
    })));

    const { result } = renderHook(() => {
      const llm = useChatModelSettings({ activeConversation: FRESH_CONVERSATION, projectId: '9', userId: '5' });
      return useChatBoxModelSelection({
        projectId: '9', selectedModelName: 'E2E-MOCK-MODEL', setSelectedModel: vi.fn(), llm,
      });
    }, { wrapper });

    await waitFor(() => expect(result.current.selectedLlmModel?.name).toBe('E2E-MOCK-MODEL'));
  });

  it('refuses a same-named model from a different numeric project', async () => {
    server.use(http.get(`${BASE}/configurations/models/9`, () => HttpResponse.json({
      items: [{ name: 'E2E-MOCK-MODEL', project_id: 1, default: true }],
    })));

    const { result } = renderHook(() => useChatBoxModelSelection({
      projectId: '9', selectedModelName: 'E2E-MOCK-MODEL', setSelectedModel: vi.fn(),
      llm: { settings: { model_name: 'E2E-MOCK-MODEL', model_project_id: 9 } },
    }), { wrapper });

    await waitFor(() => expect(result.current.modelsList).toHaveLength(1));
    expect(result.current.selectedLlmModel).toBeNull();
  });

  it('restores the persisted picked model after a terminal conversation refresh', async () => {
    server.use(http.get(`${BASE}/configurations/models/9`, () => HttpResponse.json({
      items: [
        { name: 'default-model', project_id: 9, default: true },
        { name: 'eu.anthropic.claude-haiku', project_id: 1 },
      ],
    })));
    const activeConversation = {
      id: '777', meta: { steps_limit: 25 },
      participants: [
        { id: 1, entity_name: 'user', entity_meta: { id: 5 }, entity_settings: {} },
        { id: 2, entity_name: 'dummy', entity_meta: {}, entity_settings: {
          llm_settings: { model_name: 'eu.anthropic.claude-haiku', model_project_id: 1, stream: true },
        } },
      ],
    };
    const { result, rerender } = renderHook(({ conversation }) => {
      const llm = useChatModelSettings({ activeConversation: conversation, projectId: '9', userId: '5' });
      return useChatBoxModelSelection({
        projectId: '9', selectedModelName: 'default-model', setSelectedModel: vi.fn(), llm,
      });
    }, { wrapper, initialProps: { conversation: activeConversation } });
    await waitFor(() => expect(result.current.selectedLlmModel?.name).toBe('eu.anthropic.claude-haiku'));
    rerender({ conversation: structuredClone(activeConversation) });
    await waitFor(() => expect(result.current.selectedLlmModel?.name).toBe('eu.anthropic.claude-haiku'));
  });
});

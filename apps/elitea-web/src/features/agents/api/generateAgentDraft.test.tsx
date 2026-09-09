import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { ApplicationDraft } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useGenerateAgentDraftMutation } from './generateAgentDraft';

/**
 * The endpoint is served (#254 P1). This local factory keeps the default draft
 * in one place so each test names only what it asserts on.
 */
const DEFAULT_DRAFT: ApplicationDraft = {
  name: 'Support Bot',
  description: 'Answers support questions',
  instructions: 'draft',
  welcome_message: '',
  conversation_starters: [],
  suggested_toolkits: [],
  suggested_mcp: [],
  suggested_pipelines: [],
  suggested_agents: [],
  suggested_skills: [],
};

function generateAgentDraftHandler(body?: Partial<ApplicationDraft>) {
  return http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', () =>
    HttpResponse.json({ ...DEFAULT_DRAFT, ...body }),
  );
}

function createWrapper(): ({ children }: { children: ReactNode }) => ReactNode {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useGenerateAgentDraftMutation', () => {
  it('sends user_description and returns the structured draft', async () => {
    // The `user_description -> input` mapping this hook used to make existed
    // only because the URL reached the GENERIC predictor, which has no
    // user_description field. The served endpoint reads user_description, so
    // sending `input` would now be a request the handler refuses with 400.
    let capturedBody: unknown;
    server.use(
      http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({
          name: 'Support Bot',
          description: 'Answers support questions',
          instructions: 'Be brief.',
          welcome_message: 'How can I help?',
          conversation_starters: ['Where are my orders?'],
        });
      }),
    );

    const { result } = renderHook(() => useGenerateAgentDraftMutation(), { wrapper: createWrapper() });

    let response;
    await act(async () => {
      response = await result.current.generateDraft({ projectId: 'proj-1', user_description: 'a support bot' });
    });

    expect(capturedBody).toStrictEqual({ user_description: 'a support bot' });
    expect(response).toStrictEqual({
      name: 'Support Bot',
      description: 'Answers support questions',
      instructions: 'Be brief.',
      welcome_message: 'How can I help?',
      conversation_starters: ['Where are my orders?'],
    });
    expect(result.current.isLoading).toBe(false);
    expect(result.current.error).toBeUndefined();
  });

  it('captures the error and returns undefined on a 400/403 rejection', async () => {
    server.use(
      http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'bad request' }, { status: 400 }),
      ),
    );

    const { result } = renderHook(() => useGenerateAgentDraftMutation(), { wrapper: createWrapper() });

    let response;
    await act(async () => {
      response = await result.current.generateDraft({ projectId: 'proj-1', user_description: 'x' });
    });

    expect(response).toBeUndefined();
    expect(result.current.error).toBeDefined();
  });

  it('reset() clears a previously captured error', async () => {
    server.use(
      http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'bad request' }, { status: 400 }),
      ),
    );
    const { result } = renderHook(() => useGenerateAgentDraftMutation(), { wrapper: createWrapper() });
    await act(async () => {
      await result.current.generateDraft({ projectId: 'proj-1', user_description: 'x' });
    });
    expect(result.current.error).toBeDefined();

    act(() => result.current.reset());
    expect(result.current.error).toBeUndefined();
  });

  it('default MSW handler round-trip works end-to-end', async () => {
    server.use(generateAgentDraftHandler());
    const { result } = renderHook(() => useGenerateAgentDraftMutation(), { wrapper: createWrapper() });
    let response;
    await act(async () => {
      response = await result.current.generateDraft({ projectId: 'proj-1', user_description: 'anything' });
    });
    expect(response).toBeDefined();
  });
});

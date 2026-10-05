import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  AGENT_CONTINUE_STATIC_CONTRACT,
  continueAgentExecution,
  conversationCreate,
  conversationDetails,
  conversationEdit,
  deleteConversation,
  regenerate,
  selectConversation,
  startAgentExecution,
  stopChatTask,
  unselectConversation,
  useConversationCreateMutation,
} from './conversationApi';

const BASE = '/api/v2';

function createWrapper(): ({ children }: { children: ReactNode }) => ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('conversationCreate', () => {
  it('POSTs to elitea_core/conversations/prompt_lib/{projectId} with the body sans projectId', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ id: 1, name: 'New' });
      }),
    );
    const result = await conversationCreate({ projectId: 7, name: 'New', is_private: true });
    expect(result).toEqual({ id: 1, name: 'New' });
    expect(capturedBody).toEqual({ name: 'New', is_private: true });
  });
});

describe('useConversationCreateMutation', () => {
  it('resolves via the hook', async () => {
    server.use(http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, () => HttpResponse.json({ id: 1, name: 'New' })));
    const { result } = renderHook(() => useConversationCreateMutation(), { wrapper: createWrapper() });
    result.current.mutate({ projectId: 7, name: 'New', is_private: true });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toEqual({ id: 1, name: 'New' });
  });
});

describe('conversationEdit', () => {
  it('PUTs to elitea_core/conversation/prompt_lib/{projectId}/{id}', async () => {
    server.use(http.put(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, () => HttpResponse.json({ id: 1, name: 'Renamed' })));
    const result = await conversationEdit({ projectId: 7, id: 1, name: 'Renamed' });
    expect(result).toEqual({ id: 1, name: 'Renamed' });
  });
});

describe('deleteConversation', () => {
  it('DELETEs elitea_core/conversation/prompt_lib/{projectId}/{id}', async () => {
    server.use(http.delete(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, () => HttpResponse.json({})));
    await expect(deleteConversation({ projectId: 7, id: 1 })).resolves.toEqual({});
  });
});

describe('conversationDetails', () => {
  it('GETs elitea_core/conversation/prompt_lib/{projectId}/{id} with no query when no optional params are given', async () => {
    let capturedUrl = '';
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, ({ request }) => {
        capturedUrl = request.url;
        return HttpResponse.json({ id: 1, name: 'Conv' });
      }),
    );
    const result = await conversationDetails({ projectId: 7, id: 1 });
    expect(result).toEqual({ id: 1, name: 'Conv' });
    expect(capturedUrl).not.toContain('?');
  });

  it('includes messages_offset/messages_limit/sort_order when given', async () => {
    let capturedUrl = '';
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, ({ request }) => {
        capturedUrl = request.url;
        return HttpResponse.json({ id: 1 });
      }),
    );
    await conversationDetails({ projectId: 7, id: 1, messages_offset: 0, messages_limit: 10, sort_order: 'desc' });
    expect(capturedUrl).toContain('messages_offset=0');
    expect(capturedUrl).toContain('messages_limit=10');
    expect(capturedUrl).toContain('sort_order=desc');
  });
});

describe('selectConversation / unselectConversation', () => {
  it('POSTs an empty body to select_conversation', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/select_conversation/prompt_lib/7/1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({});
      }),
    );
    await selectConversation({ projectId: 7, conversationId: 1 });
    expect(capturedBody).toEqual({});
  });

  it('DELETEs select_conversation with no conversation id', async () => {
    server.use(http.delete(`${BASE}/elitea_core/select_conversation/prompt_lib/7`, () => HttpResponse.json({})));
    await expect(unselectConversation({ projectId: 7 })).resolves.toEqual({});
  });
});

describe('regenerate', () => {
  it('POSTs elitea_core/regenerate/prompt_lib/{projectId}/{id}', async () => {
    server.use(http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/1`, () => HttpResponse.json({ ok: true })));
    await expect(regenerate({ projectId: 7, id: 1 })).resolves.toEqual({ ok: true });
  });
});

describe('regenerate — SSE contract (issue #93)', () => {
  it('sends execution_contract as a QUERY parameter and keeps it out of the body', async () => {
    let seenUrl: string | undefined;
    let seenBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/1`, async ({ request }) => {
        seenUrl = request.url;
        seenBody = await request.json();
        return HttpResponse.json({ events_url: '/api/v2/executions/7/exec-1/events' });
      }),
    );

    await expect(regenerate({ projectId: 7, id: 1, executionContract: 'agent.regenerate.v1', question: 'redo' })).resolves.toEqual({
      events_url: '/api/v2/executions/7/exec-1/events',
    });
    expect(new URL(String(seenUrl)).searchParams.get('execution_contract')).toBe('agent.regenerate.v1');
    expect(seenBody).toEqual({ question: 'redo' });
  });

  it('omits the query entirely when no contract is requested (pre-#93 behaviour)', async () => {
    let seenUrl: string | undefined;
    server.use(
      http.post(`${BASE}/elitea_core/regenerate/prompt_lib/7/1`, ({ request }) => {
        seenUrl = request.url;
        return HttpResponse.json({ ok: true });
      }),
    );
    await regenerate({ projectId: 7, id: 1 });
    expect(new URL(String(seenUrl)).search).toBe('');
  });
});

describe('startAgentExecution (issue #93)', () => {
  it('POSTs elitea_core/messages/prompt_lib/{projectId}/{conversationUuid} with the execution contract, returning events_url', async () => {
    let seenUrl: string | undefined;
    let seenBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, async ({ request }) => {
        seenUrl = request.url;
        seenBody = await request.json();
        return HttpResponse.json({ task_id: 'exec-1', events_url: '/api/v2/executions/7/exec-1/events' });
      }),
    );

    await expect(
      startAgentExecution({ projectId: 7, conversationUuid: 'uuid-1', contract: 'agent.execute.application.v1', body: { question: 'hi' } }),
    ).resolves.toMatchObject({ events_url: '/api/v2/executions/7/exec-1/events' });
    expect(new URL(String(seenUrl)).searchParams.get('execution_contract')).toBe('agent.execute.application.v1');
    expect(seenBody).toEqual({ question: 'hi' });
  });

  it('rejects when the route rejects the contract — the caller decides to fall back', async () => {
    server.use(http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, () => new HttpResponse(null, { status: 400 })));
    await expect(
      startAgentExecution({ projectId: 7, conversationUuid: 'uuid-1', contract: 'nope', body: {} }),
    ).rejects.toThrow();
  });
});

describe('stopChatTask', () => {
  it('DELETEs the same route as pipelines.stopLlmTask (elitea_core/task/prompt_lib/{projectId}/{taskId})', async () => {
    server.use(http.delete(`${BASE}/elitea_core/task/prompt_lib/7/mg-1`, () => HttpResponse.json({})));
    await expect(stopChatTask({ projectId: 7, messageGroupUuid: 'mg-1' })).resolves.toEqual({});
  });
});

describe('continueAgentExecution — generated static continuation contract', () => {
  const conversationUuid = '09a6af75-9249-4ee6-90a4-bd4015b3dfd9';
  const messageId = 'fd3852cd-2c52-46e7-b213-88de54fae6ed';
  const rootBody = {
    project_id: 7,
    conversation_uuid: conversationUuid,
    message_id: messageId,
    static_pause_id: `pipeline-static:sha256:${'a'.repeat(64)}`,
    user_input: 'Continue the original occurrence',
  };
  const toolsBody = {
    project_id: 7,
    conversation_uuid: conversationUuid,
    message_id: messageId,
    static_decisions: [{
      pause_id: rootBody.static_pause_id,
      child_thread_id: 'original-child',
      tool_call_id: 'original-call',
      action: 'continue',
      value: 'Continue the selected original leaf',
    }],
  };
  const receipt = {
    task_id: 'original-task',
    execution_id: 'original-execution',
    command_id: 'continuation-command',
    response_message_id: messageId,
    events_url: '/api/v2/executions/7/original-execution/events',
    created: true,
  };
  const params = (body: Readonly<Record<string, unknown>>) => ({
    projectId: 7,
    conversationUuid,
    contract: AGENT_CONTINUE_STATIC_CONTRACT,
    body,
  });
  const route = `${BASE}/elitea_core/continue_predict/prompt_lib/7/${conversationUuid}`;

  it.each([['root', rootBody], ['selected leaves', toolsBody]] as const)('sends the exact closed %s body once', async (_name, body) => {
    const calls: { body: unknown; contract: string | null; contentType: string | null }[] = [];
    server.use(http.post(route, async ({ request }) => {
      calls.push({
        body: await request.json(),
        contract: new URL(request.url).searchParams.get('execution_contract'),
        contentType: request.headers.get('Content-Type'),
      });
      return HttpResponse.json(receipt);
    }));
    await expect(continueAgentExecution(params(body))).resolves.toEqual(receipt);
    expect(calls).toEqual([{ body, contract: AGENT_CONTINUE_STATIC_CONTRACT, contentType: 'application/json' }]);
  });

  it.each([
    ['extra root field', { ...rootBody, checkpoint_id: 'browser-supplied' }],
    ['extra leaf field', { ...toolsBody, static_decisions: [{ ...toolsBody.static_decisions[0], checkpoint_id: 'browser-supplied' }] }],
    ['different project', { ...rootBody, project_id: 8 }],
    ['different conversation', { ...rootBody, conversation_uuid: '543d761e-edb4-451a-9671-50c272b3c295' }],
  ] as const)('refuses %s before a request', async (_name, body) => {
    let calls = 0;
    server.use(http.post(route, () => { calls++; return HttpResponse.json(receipt); }));
    await expect(continueAgentExecution(params(body))).rejects.toThrow();
    expect(calls).toBe(0);
  });

  it('refuses a receipt for another response after one request', async () => {
    let calls = 0;
    server.use(http.post(route, () => {
      calls++;
      return HttpResponse.json({ ...receipt, response_message_id: '543d761e-edb4-451a-9671-50c272b3c295' });
    }));
    await expect(continueAgentExecution(params(rootBody))).rejects.toThrow('Static continuation response mismatch');
    expect(calls).toBe(1);
  });

  it('keeps a rejected static request from retrying under another contract', async () => {
    const contracts: (string | null)[] = [];
    server.use(http.post(route, ({ request }) => {
      contracts.push(new URL(request.url).searchParams.get('execution_contract'));
      return HttpResponse.json({ error: 'static_pause_already_resolved' }, { status: 409 });
    }));
    await expect(continueAgentExecution(params(rootBody))).rejects.toThrow();
    expect(contracts).toEqual([AGENT_CONTINUE_STATIC_CONTRACT]);
  });
});

import { act, useState } from 'react';

import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import type { TestSocketClient } from '@/shared/api/socket/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { installWebStorageShim } from '@/test/webstorage';

import { createTestQueryClient } from '../../__tests__/testUtils';
import { useToolkitChat } from './useToolkitChat.hooks';
import type { UseToolkitChatParams, UseToolkitChatResult } from './useToolkitChat.types';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;

// A "run finished" streaming message reaching `indexChatReducer.local.ts`'s
// `applyStreamingUpdate` calls `notifyTaskComplete()`
// (`soundNotification.local.ts`), which reads localStorage — under this
// vitest project, Node's own experimental `localStorage` global shadows
// jsdom's (see `src/test/webstorage.ts`'s own doc comment), so
// `window.localStorage` is `undefined` unless shimmed. `document.hasFocus()`
// returning `true` (this jsdom default) already makes `notifyTaskComplete`
// a no-op for MOST of this file's tests, but is not guaranteed across every
// jsdom/vitest version — installing the shim is the sanctioned, harmless
// fix regardless.
installWebStorageShim();

function baseParams(overrides: Partial<UseToolkitChatParams> = {}): UseToolkitChatParams {
  return {
    toolkitId: 'tk-1',
    runTool: 'search_index',
    isValidForm: true,
    toolInputVariables: { query: 'x' },
    index: undefined,
    traceNewIndex: vi.fn(),
    refetchIndexesList: vi.fn(),
    cancelIndexingCallback: vi.fn(),
    values: { type: 'github', settings: { repo: 'x' } },
    modes: [],
    onMcpAuthRequired: vi.fn(),
    modelList: [{ name: 'gpt-4o-mini', default: true }],
    defaultModel: { name: 'gpt-4o-mini', default: true },
    createConversation: vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } }),
    addParticipant: vi.fn().mockResolvedValue({ data: [{ entity_name: 'toolkit', entity_meta: { id: 'tk-1' } }] }),
    stopIndexing: vi.fn().mockResolvedValue(undefined),
    buildMessagePayload: vi.fn().mockReturnValue({ user_input: 'x' }),
    onSuccess: vi.fn(),
    onError: vi.fn(),
    ...overrides,
  };
}

/** `useToolkitChat` bottoms out at `useIndexHistory` (router + query-client + zustand) and `useSocketClient` (socket context). */
function renderToolkitChat(
  params: UseToolkitChatParams,
  client: TestSocketClient = createTestSocketClient(),
  projectId = 'proj-1',
): { readonly box: { current: UseToolkitChatResult | undefined } } {
  const box: { current: UseToolkitChatResult | undefined } = { current: undefined };

  function ProbeComponent() {
    box.current = useToolkitChat(params);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={client}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });

  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  return { box };
}

/**
 * Same stack as `renderToolkitChat`, but the params object lives in React
 * state so a test can push a NEW params object mid-test (via
 * `box.setParams`) and observe how the hook reacts to a genuine prop
 * change — needed for the `defaultModel`-arrives-after-mount effect, which
 * only fires on a re-render, not on initial mount (the initial
 * `useState(defaultModel)` already covers the synchronous case).
 */
function renderToolkitChatWithRerender(
  initialParams: UseToolkitChatParams,
  client: TestSocketClient = createTestSocketClient(),
  projectId = 'proj-1',
): { readonly box: { current: UseToolkitChatResult | undefined; setParams: (next: UseToolkitChatParams) => void } } {
  const box: { current: UseToolkitChatResult | undefined; setParams: (next: UseToolkitChatParams) => void } = {
    current: undefined,
    setParams: () => undefined,
  };

  function ProbeComponent() {
    const [params, setParams] = useState(initialParams);
    box.setParams = setParams;
    box.current = useToolkitChat(params);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={client}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });

  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  return { box };
}

/** `startIndexExecution`'s route (issue #93) — POST test_toolkit_tool. Only ever reached for `index_data` (`useToolkitChatDispatch.hooks.ts`'s "Decision 1"). */
const START_PATH = `${BASE}/elitea_core/test_toolkit_tool/prompt_lib/proj-1`;

/** `testToolkitTool`'s route (`../../api/toolkitTestRun.ts`) — reached by every OTHER tool (`baseParams`'s default `runTool: 'search_index'` included). `toolkitId` here is `baseParams`'s default `'tk-1'`. */
const TEST_TOOL_PATH = `${BASE}/elitea_core/test_tool/prompt_lib/proj-1/tk-1`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // Default for every test that is not ABOUT the SSE dispatch: a backend
  // that answers without a `task_id`, i.e. the socket.io fallback. Making
  // that explicit (rather than letting the POST 404 through MSW's
  // unhandled-request error) is what keeps the fallback assertions below
  // honest — they must pass because the response says "no execution to
  // follow", not because the request blew up.
  server.use(http.post(START_PATH, () => HttpResponse.json({})));
  // Default for every test that is not ABOUT the REST test-tool outcomes
  // below: a settled, empty success. Individual tests override this with
  // `server.use(...)` for the outcome they are actually asserting on.
  server.use(http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: true, result: {}, tool_name: 'search_index', task_id: '' })));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useToolkitChat', () => {
  it('seeds chatHistory with the mode-appropriate welcome message', async () => {
    const { box } = renderToolkitChat(baseParams({ modes: ['test_tools'] }));
    await waitFor(() => expect(box.current).toBeDefined());
    expect(box.current?.chatHistory).toHaveLength(1);
    expect(box.current?.chatHistory[0]?.content).toContain('Welcome!');
  });

  it('adopts defaultModel once it resolves (selectedModel starts at defaultModel already here since it is supplied synchronously)', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());
    expect(box.current?.selectedModel).toEqual({ name: 'gpt-4o-mini', default: true });
  });

  it('onSelectModel updates selectedModel and resets llmSettings to the model-appropriate defaults (no top_k, no reasoning_effort for a non-reasoning model) (R2 regression guard)', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.onSetLLMSettings({ temperature: 0.9 });
    });
    await waitFor(() => expect(box.current?.llmSettings.temperature).toBe(0.9));

    act(() => {
      box.current?.onSelectModel({ name: 'gpt-4' });
    });

    await waitFor(() => expect(box.current?.selectedModel).toEqual({ name: 'gpt-4' }));
    expect(box.current?.llmSettings).toEqual({ temperature: 0.6, max_tokens: -1 });
  });

  it('onSelectModel includes reasoning_effort (never top_k) when the newly-selected model supports reasoning (R2 regression guard)', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.onSelectModel({ name: 'o1', supports_reasoning: true });
    });

    await waitFor(() => expect(box.current?.selectedModel).toEqual({ name: 'o1', supports_reasoning: true }));
    expect(box.current?.llmSettings).toEqual({ temperature: 0.6, max_tokens: -1, reasoning_effort: 'medium' });
  });

  it('seeds the initial llmSettings from defaultModel (reasoning_effort present when the initial default model supports reasoning) (R2 regression guard)', async () => {
    const { box } = renderToolkitChat(baseParams({ defaultModel: { name: 'o1', default: true, supports_reasoning: true } }));
    await waitFor(() => expect(box.current).toBeDefined());

    expect(box.current?.llmSettings).toEqual({ temperature: 0.6, max_tokens: -1, reasoning_effort: 'medium' });
  });

  it('handleClearActiveConversation clears the active conversation and unlocks progressing-history recovery', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleClearActiveConversation();
    });

    await waitFor(() => expect(box.current?.activeConversation).toBeNull());
  });

  it('stopRunOnIndexChange stops the current run and unlocks progressing-history recovery', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => box.current?.handleRunTool());
    await waitFor(() => expect(box.current?.isRunning).toBe(true));

    act(() => {
      box.current?.stopRunOnIndexChange();
    });

    await waitFor(() => expect(box.current?.isRunning).toBe(false));
  });

  it('handleClearChat resets chatHistory to a single fresh welcome message', async () => {
    const { box } = renderToolkitChat(baseParams());
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleClearChat();
    });

    await waitFor(() => expect(box.current?.chatHistory).toHaveLength(1));
  });

  it('handleRunTool creates a conversation and POSTs the REST test-tool body (tool_name/tool_params), never chat_predict', async () => {
    const client = createTestSocketClient();
    const createConversation = vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } });
    const addParticipant = vi.fn().mockResolvedValue({ data: [{ entity_name: 'toolkit', entity_meta: { id: 'tk-1' } }] });
    let testToolBody: unknown;
    server.use(
      http.post(TEST_TOOL_PATH, async ({ request }) => {
        testToolBody = await request.json();
        return HttpResponse.json({ ok: true, result: { hits: 3 }, tool_name: 'search_index' });
      }),
    );

    const { box } = renderToolkitChat(baseParams({ createConversation, addParticipant }), client);
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleRunTool();
    });

    await waitFor(() => expect(createConversation).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(testToolBody).toEqual({ request_id: expect.any(String) as unknown, tool_name: 'search_index', tool_params: { query: 'x' }, llm_model: 'gpt-4o-mini', llm_settings: { temperature: 0.6, max_tokens: -1 } }));
    expect(client.getEmitted('chat_predict')).toHaveLength(0);
  });

  /**
   * The REST test-tool run (`../../api/toolkitTestRun.ts`) never touches the
   * socket at all — connection state is irrelevant to it, unlike the OLD
   * `chat_predict`-only path this replaces (`./useToolkitChatDispatch.hooks.ts`'
   * "fourth case", which still stands for `index_data`).
   */
  it('runs a non-index_data tool over REST regardless of socket connection state, and never touches the socket', async () => {
    const client = createTestSocketClient();
    client.setConnectionState('disconnected');
    const onError = vi.fn();

    const { box } = renderToolkitChat(baseParams({ onError }), client);
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleRunTool();
    });

    await waitFor(() => expect(box.current?.isRunning).toBe(false));
    expect(onError).not.toHaveBeenCalled();
    expect(client.getEmitted('chat_predict')).toHaveLength(0);
  });

  /**
   * The four outcomes `services/elitea-main/internal/api/v2/toolkitrun/
   * response.go` maps a settled REST test-tool run onto — see
   * `../../api/toolkitTestRun.ts`'s own module doc comment for the exact
   * status/body pairing this exercises against.
   */
  describe('REST test-tool outcomes (every tool but index_data)', () => {
    it('OK (200 ok:true): appends the result to the transcript, no Snackbar', async () => {
      const onError = vi.fn();
      server.use(http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: true, result: { hits: 3 }, tool_name: 'search_index' })));
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(String(box.current?.chatHistory.at(-1)?.content)).toContain('"hits": 3');
      expect(onError).not.toHaveBeenCalled();
    });

    it('TOOL_ERROR (200 ok:false): appends the tool\'s own sentence to the transcript, no Snackbar', async () => {
      const onError = vi.fn();
      server.use(http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: false, error: 'the API key was rejected', tool_name: 'search_index' })));
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(String(box.current?.chatHistory.at(-1)?.content)).toContain('the API key was rejected');
      expect(onError).not.toHaveBeenCalled();
    });

    it('UNSUPPORTED_TOOLKIT (422): reports the refusal on the Snackbar, no transcript message', async () => {
      const onError = vi.fn();
      server.use(
        http.post(TEST_TOOL_PATH, () =>
          HttpResponse.json({ ok: false, reason: 'unsupported_toolkit', error: 'this image cannot build "custom"' }, { status: 422 }),
        ),
      );
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
      expect(String(onError.mock.calls[0]?.[0])).toContain('this image cannot build "custom"');
      // `startNewToolkitConversation` already cleared the welcome message
      // before dispatch (`setChatHistory([])`); a refusal appends nothing.
      expect(box.current?.chatHistory).toHaveLength(0);
    });

    it('UNKNOWN_TOOL (422): reports the refusal on the Snackbar, no transcript message', async () => {
      const onError = vi.fn();
      server.use(
        http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: false, reason: 'unknown_tool', error: 'no tool named "search_index"' }, { status: 422 })),
      );
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
      expect(String(onError.mock.calls[0]?.[0])).toContain('no tool named "search_index"');
    });

    it('bounded wait passed (504): reports the task id on the Snackbar so the caller can poll', async () => {
      const onError = vi.fn();
      server.use(
        http.post(TEST_TOOL_PATH, () =>
          HttpResponse.json({ ok: false, task_id: 'job-42', reason: 'timeout', error: 'the tool did not finish within the bounded wait' }, { status: 504 }),
        ),
      );
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
      expect(String(onError.mock.calls[0]?.[0])).toContain('job-42');
    });

    it('an unrecognised server failure (500) still reaches the Snackbar rather than being swallowed', async () => {
      const onError = vi.fn();
      server.use(http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: false, error: 'unexpected' }, { status: 500 })));
      const { box } = renderToolkitChat(baseParams({ onError }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
    });
  });

  it('does not run when isValidForm is false and the tool is not the indexing tool', async () => {
    const client = createTestSocketClient();
    const createConversation = vi.fn();
    const { box } = renderToolkitChat(baseParams({ isValidForm: false, createConversation }), client);
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleRunTool();
    });

    expect(createConversation).not.toHaveBeenCalled();
    expect(client.getEmitted('chat_predict')).toHaveLength(0);
  });

  it('stops running (without throwing out of executeRunTool) when createConversation rejects — createToolkitConversationWithParticipant swallows the error itself and resolves to null', async () => {
    // Faithful to the baseline: `createToolkitConversation`'s own try/catch
    // (`createToolkitConversationWithParticipant`) catches the rejection,
    // sets `isRunning(false)`, and resolves to `null` — `executeRunTool`'s
    // OUTER catch never sees this error, so no error chat message is
    // appended; the flow just continues with `currentConversation: null`.
    const client = createTestSocketClient();
    const createConversation = vi.fn().mockRejectedValue(new Error('network down'));
    const { box } = renderToolkitChat(baseParams({ createConversation }), client);
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleRunTool();
    });

    await waitFor(() => expect(box.current?.isRunning).toBe(false));
    expect(createConversation).toHaveBeenCalledTimes(1);
    // The REST test-tool run does not depend on `currentConversation` at all
    // (`../../api/toolkitTestRun.ts` takes no conversation input), so it
    // still runs with a null one; no exception propagates and no socket
    // emit ever fires.
    expect(client.getEmitted('chat_predict')).toHaveLength(0);
  });

  it('records a chat-history error message when the dispatch step itself throws (buildMessagePayload throws on the index_data path)', async () => {
    // `buildMessagePayload` is only called on the `index_data` branch now
    // (`useToolkitChatDispatch.hooks.ts`'s "fifth case" — every other tool
    // never builds this payload at all), so this exercises `handleIndexData`.
    const client = createTestSocketClient();
    const buildMessagePayload = vi.fn().mockImplementation(() => {
      throw new Error('payload build failed');
    });
    const { box } = renderToolkitChat(baseParams({ buildMessagePayload }), client);
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.handleIndexData();
    });

    await waitFor(() => expect(box.current?.isRunning).toBe(false));
    const lastMessage = box.current?.chatHistory.at(-1);
    expect(String(lastMessage?.content)).toContain('payload build failed');
    expect(client.getEmitted('chat_predict')).toHaveLength(0);
  });

  it('onCancelIndexing calls stopIndexing, reports success, and invokes the tab-switch callback', async () => {
    const stopIndexing = vi.fn().mockResolvedValue(undefined);
    const onSuccess = vi.fn();
    const cancelIndexingCallback = vi.fn();
    const index = { id: 'idx-1', metadata: { collection: 'my-index', task_id: 't1', state: 'in_progress' } };

    const { box } = renderToolkitChat(baseParams({ index, stopIndexing, onSuccess, cancelIndexingCallback }));
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.onCancelIndexing();
    });

    await waitFor(() => expect(stopIndexing).toHaveBeenCalledWith({ projectId: 'proj-1', toolkitId: 'tk-1', indexName: 'my-index', taskId: 't1' }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalledWith('Indexing stopped successfully'));
    expect(cancelIndexingCallback).toHaveBeenCalledWith('configuration');
  });

  it('onCancelIndexing reports the error instead when stopIndexing rejects', async () => {
    const stopIndexing = vi.fn().mockRejectedValue(new Error('stop failed'));
    const onError = vi.fn();
    const index = { id: 'idx-1', metadata: { collection: 'my-index', task_id: 't1', state: 'in_progress' } };

    const { box } = renderToolkitChat(baseParams({ index, stopIndexing, onError }));
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.onCancelIndexing();
    });

    await waitFor(() => expect(onError).toHaveBeenCalledWith('Failed to stop indexing'));
  });

  it('onCancelIndexing does nothing (no stopIndexing call) when there is no index', async () => {
    const stopIndexing = vi.fn();
    const { box } = renderToolkitChat(baseParams({ index: undefined, stopIndexing }));
    await waitFor(() => expect(box.current).toBeDefined());

    act(() => {
      box.current?.onCancelIndexing();
    });

    expect(stopIndexing).not.toHaveBeenCalled();
  });

  describe('resolveRunInputVariables / handleIndexData (the indexing tool)', () => {
    it('uses index.metadata.index_configuration as the tool input, and traces the "in_progress" state, when indexing outside create-index mode with an index present', async () => {
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const createConversation = vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } });
      const index = { id: 'idx-1', metadata: { state: 'created', index_configuration: { foo: 'bar' } } };

      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex, createConversation }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => {
        box.current?.handleIndexData();
      });

      await waitFor(() => expect(createConversation).toHaveBeenCalledTimes(1));
      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', expect.objectContaining({ collection: undefined, state: 'in_progress' }));
      await waitFor(() => expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { conversation_id: 'conv-1' }));
    });

    // issue 310: "a start is gated when one is already active" — the
    // index's own last-known server metadata says it is already indexing,
    // even though nothing in THIS render's local state (`isRunning`) knows
    // that yet (no `conversation_id`, so the recovery effect never fires).
    it('does not start a new run when the index is already actively indexing per server metadata', async () => {
      const client = createTestSocketClient();
      const createConversation = vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } });
      const index = { id: 'idx-1', metadata: { state: 'in_progress' } };

      const { box } = renderToolkitChat(baseParams({ modes: [], index, createConversation }), client);
      await waitFor(() => expect(box.current).toBeDefined());
      expect(box.current?.isRunning).toBe(false);

      act(() => {
        box.current?.handleIndexData();
      });

      expect(createConversation).not.toHaveBeenCalled();
      expect(box.current?.isRunning).toBe(false);
    });

    /**
     * `run()`'s own `canProceed` gate (`!isRunning`) blocks a second
     * `handleIndexData()` call while a prior run is still in flight — both
     * tests below drive a real socket "finish" (start_task, then a
     * streaming-update message carrying `response_metadata.finish_reason`)
     * between the two calls to legitimately flip `isRunning` back to
     * `false` first, exactly like a real completed run would.
     */
    function finishActiveRun(client: TestSocketClient, messageId: string): void {
      act(() => {
        client.simulateServerEvent('chat_predict', { message_id: messageId, type: 'start_task', content: { task_id: messageId } });
      });
      act(() => {
        client.simulateServerEvent('chat_predict', {
          message_id: messageId,
          type: 'agent_response',
          content: 'done',
          response_metadata: { finish_reason: 'stop' },
        });
      });
    }

    it('creates a fresh conversation on every handleIndexData call outside test-tools mode, even with an existing activeConversation', async () => {
      const client = createTestSocketClient();
      const createConversation = vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } });
      const { box } = renderToolkitChat(baseParams({ modes: [], createConversation }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(createConversation).toHaveBeenCalledTimes(1));
      finishActiveRun(client, 'run-1');
      await waitFor(() => expect(box.current?.isRunning).toBe(false));

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(createConversation).toHaveBeenCalledTimes(2));
    });

    it('reuses the existing activeConversation on a second handleIndexData call while in test-tools mode', async () => {
      const client = createTestSocketClient();
      const createConversation = vi.fn().mockResolvedValue({ data: { id: 'conv-1', uuid: 'uuid-1', participants: [] } });
      const { box } = renderToolkitChat(baseParams({ modes: ['test_tools'], createConversation }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(createConversation).toHaveBeenCalledTimes(1));
      finishActiveRun(client, 'run-1');
      await waitFor(() => expect(box.current?.isRunning).toBe(false));

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(2));
      // The conversation is reused, not recreated.
      expect(createConversation).toHaveBeenCalledTimes(1);
    });
  });

  describe('describeRunError (executeRunTool catch branch — exercised via handleIndexData, the only tool that still calls buildMessagePayload)', () => {
    it('records a chat-history error message built from a plain string error', async () => {
      const client = createTestSocketClient();
      const buildMessagePayload = vi.fn().mockImplementation(() => {
        // eslint-disable-next-line @typescript-eslint/only-throw-error -- deliberately exercising the non-Error catch branch
        throw 'plain string failure';
      });
      const { box } = renderToolkitChat(baseParams({ buildMessagePayload }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(String(box.current?.chatHistory.at(-1)?.content)).toContain('plain string failure');
    });

    it('records a chat-history error message built from a plain (non-Error) object, JSON-stringified', async () => {
      const client = createTestSocketClient();
      const buildMessagePayload = vi.fn().mockImplementation(() => {
        // eslint-disable-next-line @typescript-eslint/only-throw-error -- deliberately exercising the non-Error catch branch
        throw { code: 'E_BAD' };
      });
      const { box } = renderToolkitChat(baseParams({ buildMessagePayload }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(String(box.current?.chatHistory.at(-1)?.content)).toContain(JSON.stringify({ code: 'E_BAD' }));
    });

    it('falls back to "Unknown error" when the thrown value cannot be JSON.stringify-d (circular reference)', async () => {
      const client = createTestSocketClient();
      const buildMessagePayload = vi.fn().mockImplementation(() => {
        const circular: Record<string, unknown> = {};
        circular['self'] = circular;
        // eslint-disable-next-line @typescript-eslint/only-throw-error -- deliberately exercising the JSON.stringify-throws catch branch
        throw circular;
      });
      const { box } = renderToolkitChat(baseParams({ buildMessagePayload }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(String(box.current?.chatHistory.at(-1)?.content)).toContain('Unknown error');
    });

    it('traces a "failed" index state (in addition to the chat-history error message) when the throwing run was the indexing tool', async () => {
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const buildMessagePayload = vi.fn().mockImplementation(() => {
        throw new Error('boom');
      });
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex, buildMessagePayload }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', expect.objectContaining({ state: 'failed' }));
    });
  });

  describe('socket-driven onRunFinish/onStartTask (chat_predict start_task / streaming-finish messages)', () => {
    it('traces the started task (outside test-tools mode) when a start_task message arrives', async () => {
      // `handleIndexData` (not `handleRunTool`): only `index_data` still
      // rides the REST-start-then-socket-fallback path this test is about
      // (`useToolkitChatDispatch.hooks.ts`'s "fourth case") — every other
      // tool now settles over REST directly (the "fifth case").
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));

      act(() => {
        client.simulateServerEvent('chat_predict', { message_id: 'run-1', type: 'start_task', content: { task_id: 'task-99' } });
      });

      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { task_id: 'task-99' });
    });

    it('does NOT trace the started task while in test-tools mode', async () => {
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const { box } = renderToolkitChat(baseParams({ modes: ['test_tools'], traceNewIndex }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));

      act(() => {
        client.simulateServerEvent('chat_predict', { message_id: 'run-1', type: 'start_task', content: { task_id: 'task-99' } });
      });

      expect(traceNewIndex).not.toHaveBeenCalledWith(expect.anything(), { task_id: 'task-99' });
    });

    it('outside test-tools mode, once a run finishes for the indexing tool, traces the finish state and refetches the index list after the debounce', async () => {
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const refetchIndexesList = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex, refetchIndexesList }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      // `handleIndexData` (not `handleRunTool`) so `runningToolRef.current`
      // equals `IndexesToolsEnum.indexData` — the ONLY value that lets
      // `onRunFinish`'s `setTimeout` callback proceed past its own internal
      // guard instead of returning early.
      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));

      act(() => {
        client.simulateServerEvent('chat_predict', { message_id: 'run-1', type: 'start_task', content: { task_id: 'task-1' } });
      });
      act(() => {
        client.simulateServerEvent('chat_predict', {
          message_id: 'run-1',
          type: 'agent_response',
          content: 'all done',
          response_metadata: { finish_reason: 'stop' },
        });
      });

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      await waitFor(() => expect(refetchIndexesList).toHaveBeenCalledTimes(1), { timeout: 2000 });
      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { state: 'completed' });
    });

    it('does NOT refetch the index list when the finished run was a non-indexing tool (runningToolRef mismatch, setTimeout guard returns early)', async () => {
      // `handleRunTool` runs `runTool` ('search_index'), never
      // `IndexesToolsEnum.indexData` — and, since it is not `index_data`,
      // this settles over the REST test-tool run (the "fifth case"), never
      // the socket, so the finish comes from `onTestToolOutcome` directly
      // rather than a simulated `chat_predict` message.
      const client = createTestSocketClient();
      const refetchIndexesList = vi.fn();
      server.use(http.post(TEST_TOOL_PATH, () => HttpResponse.json({ ok: true, result: {}, tool_name: 'search_index' })));
      const { box } = renderToolkitChat(baseParams({ modes: [], refetchIndexesList }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      // Give the 500ms debounce time to fire and confirm it never calls through.
      await new Promise((resolve) => setTimeout(resolve, 700));
      expect(refetchIndexesList).not.toHaveBeenCalled();
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
    });
  });

  describe('recovering an in-progress index conversation on mount', () => {
    it('marks isRunning true and replaces chatHistory with the recovered conversation once the recovery fetch resolves (needGenerateProgressingIndexHistory effect)', async () => {
      // `useIndexHistory`'s own `historyMessages` used to be derived from
      // `conversationDetails` ONLY when `isHistoryMode` (a SEPARATE,
      // zustand-store-driven "History" tab concern — `selectedHistoryItem`)
      // was true; during this progressing-index-RECOVERY flow (this test's
      // own scenario) `isHistoryMode` is false, so `historyMessages` used to
      // resolve to `[]` even though the recovery fetch below genuinely
      // completed and genuinely flipped `needGenerateProgressingIndexHistory`
      // true — a real bug in `useIndexHistory.hooks.ts` (a sibling A4a file,
      // found while writing this exact test), fixed there (see that file's
      // own `useMemo`, `conversation = conversationDetails ?? null`, no
      // longer gated on `isHistoryMode`) with its own regression test in
      // `useIndexHistory.hooks.test.tsx`. This test now asserts the real,
      // correct outcome: `useToolkitChat.hooks.ts`'s own effect
      // (`useToolkitChat.hooks.ts:179-184`) fires the fetch, fires exactly
      // once, and `setChatHistory([...historyMessages])` replaces the
      // welcome-message seed with the ACTUAL recovered conversation content,
      // not an empty array.
      let hit = false;
      server.use(
        http.get(`${BASE}/elitea_core/conversation/prompt_lib/proj-1/conv-99`, () => {
          hit = true;
          return HttpResponse.json({
            message_groups: [
              {
                id: 1,
                uuid: 'u1',
                author_participant_id: 'user-1',
                content: 'recovered question',
                created_at: '2024-01-01 00:00:00',
                sent_to_id: 'toolkit-1',
              },
            ],
            participants: [{ id: 'user-1', entity_name: 'user', meta: { user_name: 'Alice' } }],
          });
        }),
      );

      const index = { id: 'idx-1', metadata: { state: 'in_progress', conversation_id: 'conv-99' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index }));

      await waitFor(() => expect(hit).toBe(true), { timeout: 3000 });
      // The welcome message (chatHistory's initial seed) starts at length 1;
      // once the recovery effect runs, `setChatHistory([...historyMessages])`
      // replaces it with the real recovered conversation content.
      await waitFor(() => expect(box.current?.chatHistory).toHaveLength(1), { timeout: 3000 });
      expect(String(box.current?.chatHistory[0]?.content)).toContain('recovered question');
      expect(box.current?.isRunning).toBe(true);
    });

    it('does not attempt recovery while in create-index mode, even with an in-progress index + conversation_id', async () => {
      let hit = false;
      server.use(
        http.get(`${BASE}/elitea_core/conversation/prompt_lib/proj-1/conv-99`, () => {
          hit = true;
          return HttpResponse.json({ message_groups: [], participants: [] });
        }),
      );
      const index = { id: 'idx-1', metadata: { state: 'in_progress', conversation_id: 'conv-99' } };
      const { box } = renderToolkitChat(baseParams({ modes: ['create_index'], index }));
      await waitFor(() => expect(box.current).toBeDefined());
      expect(hit).toBe(false);
    });
  });

  describe('defaultModel arriving after mount', () => {
    it('adopts a defaultModel that arrives on a later render, when selectedModel was still null at mount', async () => {
      const { box } = renderToolkitChatWithRerender(baseParams({ defaultModel: null }));
      await waitFor(() => expect(box.current).toBeDefined());
      expect(box.current?.selectedModel).toBeNull();

      act(() => {
        box.setParams(baseParams({ defaultModel: { name: 'gpt-4o', default: true } }));
      });

      await waitFor(() => expect(box.current?.selectedModel).toEqual({ name: 'gpt-4o', default: true }));
    });

    it('does not override an already-selected model when defaultModel changes again later', async () => {
      const { box } = renderToolkitChatWithRerender(baseParams({ defaultModel: { name: 'first', default: true } }));
      await waitFor(() => expect(box.current?.selectedModel).toEqual({ name: 'first', default: true }));

      act(() => {
        box.current?.onSelectModel({ name: 'user-picked' });
      });
      await waitFor(() => expect(box.current?.selectedModel).toEqual({ name: 'user-picked' }));

      act(() => {
        box.setParams(baseParams({ defaultModel: { name: 'second', default: true } }));
      });

      // selectedModel is no longer null, so the "adopt defaultModel" effect's own gate stays closed.
      await waitFor(() => expect(box.current).toBeDefined());
      expect(box.current?.selectedModel).toEqual({ name: 'user-picked' });
    });
  });
  /**
   * Issue #93 — SSE dispatch. The run is started over REST; when the
   * backend returns a `task_id`, `useToolkitChatSocket` follows that
   * execution's durable event stream and NOTHING is emitted on socket.io.
   * When it does not, the socket emit is still the transport.
   */
  describe('SSE execution dispatch (REST start + durable event stream)', () => {
    let sse: TestEventSourceRegistry;

    beforeEach(() => {
      sse = installTestEventSource();
      globals['elitea_ui_config'] = { vite_server_url: BASE, vite_base_uri: '/', vite_public_project_id: 'public-1' };
      resetConfigForTests();
    });

    afterEach(() => {
      sse.restore();
      delete globals['elitea_ui_config'];
      resetConfigForTests();
    });

    it('POSTs the GO contract body for an index_data run and follows the returned task_id, without emitting chat_predict', async () => {
      let startUrl: string | undefined;
      let startBody: unknown;
      server.use(
        http.post(START_PATH, async ({ request }) => {
          startUrl = request.url;
          startBody = await request.json();
          return HttpResponse.json({ task_id: 'exec-1' });
        }),
      );
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));
      expect(sse.getSources()[0]?.url).toBe(`${BASE}/executions/proj-1/exec-1/events`);
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
      expect(startUrl).toContain('await_response=false');
      expect(startUrl).toContain('execution_contract=index.ingest.v1');
      // The Go handler validates `toolkit_config` and `tool_name` before
      // anything else — the socket predict payload has neither.
      expect(startBody).toMatchObject({ toolkit_config: { toolkit_id: 'tk-1' }, tool_name: 'index_data' });
      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { task_id: 'exec-1' });
    });

    it('never attempts the REST start (nor the socket) for a non-index_data tool — it settles over the REST test-tool run instead', async () => {
      let started = false;
      let testToolCalled = false;
      server.use(
        http.post(START_PATH, () => {
          started = true;
          return HttpResponse.json({ task_id: 'exec-1' });
        }),
        http.post(TEST_TOOL_PATH, () => {
          testToolCalled = true;
          return HttpResponse.json({ ok: true, result: {}, tool_name: 'search_index' });
        }),
      );
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleRunTool());

      await waitFor(() => expect(testToolCalled).toBe(true));
      expect(started).toBe(false);
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
      expect(sse.getSources()).toHaveLength(0);
    });

    /**
     * The critical one: `task_id` alone does NOT prove the Go runtime
     * answered — legacy pylon honours `await_response=false` and returns a
     * `task_id` of its own while serving no `/executions/…/events` stream.
     * The stream failing to open is the real discriminator.
     */
    it('emits on socket.io after all when the execution stream fails to open (legacy backend returned a task_id)', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'legacy-task' })));
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));
      expect(client.getEmitted('chat_predict')).toHaveLength(0);

      act(() => {
        sse.fail();
      });

      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));
      const emitted = client.getEmitted('chat_predict')[0]?.payload as { tool_call_input: { tool_name: string } };
      expect(emitted.tool_call_input.tool_name).toBe('index_data');
      // And the stream is dropped, so nothing is left following a dead id.
      await waitFor(() => expect(sse.getOpen()).toHaveLength(0));
    });

    it('does not re-emit on a stream failure that arrives after the run already settled', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'exec-1' })));
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));
      act(() => {
        sse.emit('index.ingest.completed', JSON.stringify({ status: 'ok' }));
      });
      await waitFor(() => expect(box.current?.isRunning).toBe(false));

      act(() => {
        sse.fail();
      });
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
    });

    it('routes execution.node_event frames through the same chat reducer the socket path feeds', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'exec-1' })));
      const { box } = renderToolkitChat(baseParams({ modes: [] }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));
      // `startNewToolkitConversation` clears the welcome message, so the
      // history is empty until a frame actually lands.
      await waitFor(() => expect(box.current?.chatHistory).toHaveLength(0));

      act(() => {
        // The same frame the socket-path test uses, delivered over SSE.
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 'exec-1' } }));
      });

      await waitFor(() => expect(box.current?.chatHistory.length).toBeGreaterThan(0));
    });

    it('finishes the run on index.ingest.completed, mapping the server status onto the index state', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'exec-1' })));
      const traceNewIndex = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));

      act(() => {
        sse.emit('index.ingest.completed', JSON.stringify({ status: 'partly_indexed', message: 'some docs failed' }));
      });

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      // Terminal frame ⇒ nothing left to receive ⇒ the stream is closed.
      await waitFor(() => expect(sse.getOpen()).toHaveLength(0));
      await waitFor(() => expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { state: 'partly_indexed' }));
    });

    it('finishes the run as failed on execution.failed', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'exec-1' })));
      const traceNewIndex = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex }));
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));

      act(() => {
        sse.emit('execution.failed', JSON.stringify({ message: 'worker died' }));
      });

      await waitFor(() => expect(box.current?.isRunning).toBe(false));
      await waitFor(() => expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { state: 'failed' }));
    });

    it('falls back to the socket emit when the start call fails outright (route not mounted)', async () => {
      server.use(http.post(START_PATH, () => new HttpResponse(null, { status: 404 })));
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));
      expect(sse.getSources()).toHaveLength(0);
      // The failed start must NOT surface as a run error — the socket path
      // is carrying the run.
      expect(String(box.current?.chatHistory.at(-1)?.content)).not.toContain('Failed to execute tool');
    });

    it('falls back to the socket emit when the response carries no task_id (older backend)', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ ok: true })));
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));
      expect(sse.getSources()).toHaveLength(0);
    });

    // issue 310: the central regression test. A 409 means a run is
    // ALREADY in flight server-side; the client must ADOPT its task_id and
    // follow it, never start a second one over socket.io.
    it('adopts the task_id from a 409 conflict instead of starting a second run (issue 310)', async () => {
      server.use(
        http.post(START_PATH, () =>
          HttpResponse.json({ error: 'Indexing is already in progress for this index', task_id: 'already-running-task' }, { status: 409 }),
        ),
      );
      const client = createTestSocketClient();
      const traceNewIndex = vi.fn();
      const index = { id: 'idx-1', metadata: { state: 'created' } };
      const { box } = renderToolkitChat(baseParams({ modes: [], index, traceNewIndex }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      // DISCRIMINATES: proves the client followed the EXISTING task rather
      // than retrying — the adopted task's stream opens, and no second run
      // is ever dispatched over socket.io.
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));
      expect(sse.getSources()[0]?.url).toBe(`${BASE}/executions/proj-1/already-running-task/events`);
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
      expect(traceNewIndex).toHaveBeenCalledWith('idx-1', { task_id: 'already-running-task' });

      // And it stays that way even if the adopted stream later drops —
      // adopting must never retry.
      act(() => {
        sse.fail();
      });
      expect(client.getEmitted('chat_predict')).toHaveLength(0);
    });

    it('falls back to the socket emit for a 409 that does not match the exact conflict contract (bounds-checked task id)', async () => {
      server.use(
        http.post(START_PATH, () =>
          HttpResponse.json({ error: 'Indexing is already in progress for this index', task_id: 'bad\r\nid' }, { status: 409 }),
        ),
      );
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());

      await waitFor(() => expect(client.getEmitted('chat_predict')).toHaveLength(1));
      expect(sse.getSources()).toHaveLength(0);
    });

    // issue 310: onError must not re-dispatch a run whose stream already
    // opened — an end-to-end check that the wiring in
    // `useToolkitChatSocket.hooks.ts` actually reaches through
    // `useToolkitChat`'s own `runSocketFallback`.
    it('does not emit on socket.io when the stream drops AFTER it opened and was already carrying frames', async () => {
      server.use(http.post(START_PATH, () => HttpResponse.json({ task_id: 'exec-1' })));
      const client = createTestSocketClient();
      const { box } = renderToolkitChat(baseParams({ modes: [] }), client);
      await waitFor(() => expect(box.current).toBeDefined());

      act(() => box.current?.handleIndexData());
      await waitFor(() => expect(sse.getOpen()).toHaveLength(1));

      act(() => {
        sse.emit('open');
      });
      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 'exec-1' } }));
      });
      act(() => {
        sse.fail();
      });

      expect(client.getEmitted('chat_predict')).toHaveLength(0);
    });
  });
});

it('forwards REST authorization to TestTools callback and retries the same named operation', async () => {
  const client = createTestSocketClient();
  const onMcpAuthRequired = vi.fn();
  const bodies: Record<string, unknown>[] = [];
  server.use(http.post(`${BASE}/elitea_core/test_tool/prompt_lib/7/9`, async ({ request }) => {
    bodies.push(await request.json() as Record<string, unknown>);
    return bodies.length === 1
      ? HttpResponse.json({ reason: 'authorization_required', task_id: 'task-9', authorization_required: { toolkit_id: 9, toolkit_type: 'mcp', toolkit_name: 'Docs', server_url: 'https://mcp.example/tools', resource_metadata: {} } }, { status: 409 })
      : HttpResponse.json({ ok: true, result: 'done' });
  }));
  const { box } = renderToolkitChat(baseParams({ toolkitId: '9', onMcpAuthRequired }), client, '7');
  await waitFor(() => expect(box.current).toBeDefined());
  act(() => box.current?.handleRunTool());
  await waitFor(() => expect(onMcpAuthRequired).toHaveBeenCalledOnce());
  const message = onMcpAuthRequired.mock.calls[0]?.[0] as { onAuthorized: (reference: string) => Promise<void> };
  await act(async () => message.onAuthorized('R'.repeat(43)));
  expect(bodies).toHaveLength(2);
  expect(bodies[1]).toEqual({ ...bodies[0], request_id: expect.any(String) as unknown, mcp_authorization_reference: 'R'.repeat(43) });
  expect(bodies[1]?.['request_id']).not.toBe(bodies[0]?.['request_id']);
  expect(client.getEmitted('chat_predict')).toHaveLength(0);
  await act(async () => message.onAuthorized('R'.repeat(43)));
  expect(bodies).toHaveLength(2);
});

import type { ReactNode } from 'react';

import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import type { TestSocketClient } from '@/shared/api/socket/testing';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';

import { settleIndexRunProgress } from '../helpers/indexRunNotices.helpers';
import type { ToolkitChatMessage } from './useToolkitChat.types';
import { useToolkitChatSocket } from './useToolkitChatSocket.hooks';

function withSocket(client: TestSocketClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <SocketClientContext.Provider value={client}>{children}</SocketClientContext.Provider>;
  };
}

function setup(client: TestSocketClient, overrides: Partial<Parameters<typeof useToolkitChatSocket>[0]> = {}) {
  const onRunFinish = vi.fn();
  const onStartTask = vi.fn();
  const onMcpAuthRequired = vi.fn();
  const onStreamError = vi.fn();
  let chatHistory: ToolkitChatMessage[] = [];
  const setChatHistory = vi.fn((update: (prev: ToolkitChatMessage[]) => ToolkitChatMessage[]) => {
    chatHistory = update(chatHistory);
  });

  const { result } = renderHook(
    () =>
      useToolkitChatSocket({
        isAuthCheckSession: false,
        onMcpAuthRequired,
        onRunFinish,
        onStartTask,
        setChatHistory,
        activeConversationId: undefined,
        activeConversationUuid: undefined,
        projectId: 'proj-1',
        roomEnabled: false,
        executionId: undefined,
        onStreamError,
        ...overrides,
      }),
    { wrapper: withSocket(client) },
  );

  return { result, onRunFinish, onStartTask, onMcpAuthRequired, onStreamError, setChatHistory, getChatHistory: () => chatHistory };
}

describe('useToolkitChatSocket', () => {
  it('returns the shared socket client (for the caller\'s own chat_predict emit)', () => {
    const client = createTestSocketClient();
    const { result } = setup(client);
    expect(result.current).toBe(client);
  });

  it('routes an mcp_authorization_required message to onMcpAuthRequired instead of the chat reducer', () => {
    const client = createTestSocketClient();
    const { onMcpAuthRequired, setChatHistory } = setup(client);

    client.simulateServerEvent('chat_predict', { type: 'mcp_authorization_required', message_id: 'm1' });

    expect(onMcpAuthRequired).toHaveBeenCalledTimes(1);
    expect(setChatHistory).not.toHaveBeenCalled();
  });

  it('ignores every chat_predict message while isAuthCheckSession is true', () => {
    const client = createTestSocketClient();
    const { onMcpAuthRequired, setChatHistory } = setup(client, { isAuthCheckSession: true });

    client.simulateServerEvent('chat_predict', { type: 'mcp_authorization_required', message_id: 'm1' });
    client.simulateServerEvent('chat_predict', { type: 'chunk', message_id: 'm2' });

    expect(onMcpAuthRequired).not.toHaveBeenCalled();
    expect(setChatHistory).not.toHaveBeenCalled();
  });

  it('routes a normal chat_predict message through setChatHistory (the indexChat reducer)', () => {
    const client = createTestSocketClient();
    const { setChatHistory } = setup(client);

    client.simulateServerEvent('chat_predict', { type: 'start_task', message_id: 'm1', content: { task_id: 't1' } });

    expect(setChatHistory).toHaveBeenCalledTimes(1);
  });

  it('unsubscribes chat_predict on unmount', () => {
    const client = createTestSocketClient();
    const { result, unmount } = renderHook(
      () =>
        useToolkitChatSocket({
          isAuthCheckSession: false,
          onMcpAuthRequired: undefined,
          onRunFinish: vi.fn(),
          onStartTask: vi.fn(),
          setChatHistory: vi.fn(),
          activeConversationId: undefined,
          activeConversationUuid: undefined,
          projectId: 'proj-1',
          roomEnabled: false,
          executionId: undefined,
          onStreamError: vi.fn(),
        }),
      { wrapper: withSocket(client) },
    );
    void result;
    unmount();

    // A message after unmount must not throw and must not still be routed —
    // proven indirectly: no listener remains registered, so this is a no-op.
    expect(() => client.simulateServerEvent('chat_predict', { type: 'chunk', message_id: 'after-unmount' })).not.toThrow();
  });

  it('joins the chat_enter_room/chat_leave_rooms pair only while roomEnabled is true, using activeConversationId as the room id', () => {
    const client = createTestSocketClient();
    const { rerender } = renderHook(
      ({ roomEnabled }) =>
        useToolkitChatSocket({
          isAuthCheckSession: false,
          onMcpAuthRequired: undefined,
          onRunFinish: vi.fn(),
          onStartTask: vi.fn(),
          setChatHistory: vi.fn(),
          activeConversationId: 'conv-1',
          activeConversationUuid: 'uuid-1',
          projectId: 'proj-1',
          roomEnabled,
          executionId: undefined,
          onStreamError: vi.fn(),
        }),
      { wrapper: withSocket(client), initialProps: { roomEnabled: false } },
    );

    expect(client.getEmitted('chat_enter_room')).toHaveLength(0);

    rerender({ roomEnabled: true });
    expect(client.getEmitted('chat_enter_room')).toHaveLength(1);
    expect(client.getEmitted('chat_enter_room')[0]?.payload).toMatchObject({
      conversation_id: 'conv-1',
      conversation_uuid: 'uuid-1',
      project_id: 'proj-1',
    });

    rerender({ roomEnabled: false });
    expect(client.getEmitted('chat_leave_rooms')).toHaveLength(1);
  });
});

describe('useToolkitChatSocket — SSE execution stream (issue #93)', () => {
  const globals = globalThis as unknown as Record<string, unknown>;
  let sse: TestEventSourceRegistry;

  beforeEach(() => {
    sse = installTestEventSource();
    globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
    resetConfigForTests();
  });

  afterEach(() => {
    sse.restore();
    delete globals['elitea_ui_config'];
    resetConfigForTests();
  });

  it('opens the execution stream only once an executionId exists', () => {
    const client = createTestSocketClient();
    setup(client);
    expect(sse.getSources()).toHaveLength(0);

    setup(client, { executionId: 'exec-1' });
    expect(sse.getSources()[0]?.url).toBe('/api/v2/executions/proj-1/exec-1/events');
  });

  it('routes an execution.node_event frame through the same reducer as chat_predict', () => {
    const client = createTestSocketClient();
    const { setChatHistory } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 't1' } }));
    });

    expect(setChatHistory).toHaveBeenCalledTimes(1);
  });

  it('routes an mcp_authorization_required node event to onMcpAuthRequired, exactly like the socket path', () => {
    const client = createTestSocketClient();
    const { onMcpAuthRequired, setChatHistory } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.node_event', JSON.stringify({ type: 'mcp_authorization_required', message_id: 'm1' }));
    });

    expect(onMcpAuthRequired).toHaveBeenCalledTimes(1);
    expect(setChatHistory).not.toHaveBeenCalled();
  });

  it('drops a node event with no `type` discriminant (not a usable stream envelope)', () => {
    const client = createTestSocketClient();
    const { setChatHistory } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.node_event', JSON.stringify({ message_id: 'm1', content: 'orphan' }));
    });

    expect(setChatHistory).not.toHaveBeenCalled();
  });

  it('maps index.ingest.completed status onto the index state reported to onRunFinish', () => {
    const cases: readonly (readonly [string, string])[] = [
      ['ok', 'completed'],
      ['partly_indexed', 'partly_indexed'],
      ['error', 'failed'],
    ];
    for (const [status, expected] of cases) {
      const client = createTestSocketClient();
      const { onRunFinish } = setup(client, { executionId: 'exec-1' });
      act(() => {
        sse.emit('index.ingest.completed', JSON.stringify({ status }));
      });
      expect(onRunFinish).toHaveBeenLastCalledWith(expected);
    }
  });

  it('replaces the running placeholder with the run summary, counts and all, when the run completes (#2041)', () => {
    const client = createTestSocketClient();
    const placeholder = { id: 'index-run-progress', role: 'assistant', content: 'running', created_at: 1, participant_id: 'system' };
    const { getChatHistory } = setup(client, { executionId: 'exec-1' });
    act(() => {
      sse.emit('index.ingest.completed', JSON.stringify({ status: 'ok', message: 'Indexed 4 documents.', indexed_documents: 4, indexed_chunks: 19 }));
    });
    const history = settleIndexRunProgress([placeholder], getChatHistory().at(-1) as ToolkitChatMessage);
    expect(getChatHistory()).toHaveLength(1);
    expect(getChatHistory()[0]?.content).toContain('4 documents indexed (19 chunks)');
    expect(history.map((message) => message.id)).not.toContain('index-run-progress');
  });

  it('shows a step frame of the execution stream whose message the panel has not seen yet (#2041)', () => {
    const client = createTestSocketClient();
    const { getChatHistory } = setup(client, { executionId: 'exec-1' });
    act(() => {
      sse.emit(
        'execution.node_event',
        JSON.stringify({
          type: 'agent_tool_start',
          message_id: 'run-1',
          response_metadata: { tool_run_id: 'step-1', tool_name: 'Listing documents' },
        }),
      );
    });
    const message = getChatHistory().find((item) => item.id === 'run-1');
    expect(message).toBeDefined();
    expect((message as { toolActions?: { name?: string }[] }).toolActions?.[0]?.name).toBe('Listing documents');
  });

  it('settles as completed when the terminal frame carries no status (the artifact-shaped projection)', () => {
    const client = createTestSocketClient();
    const { onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('index.ingest.completed', JSON.stringify({ artifact_id: 'a1', media_type: 'application/json' }));
    });

    expect(onRunFinish).toHaveBeenCalledWith('completed');
  });

  // The other half of the same decision: a terminal frame that DOES carry a
  // status, but not one this build knows, must not be reported as a success.
  // `completed` is in RUNNABLE_INDEX_STATUSES, so the pre-fix `default:`
  // branch did not merely mispaint — it advertised the index as searchable on
  // the strength of a status nothing in the app has ever seen. Fails against
  // any implementation that keeps mapping "unknown" to "completed".
  it('refuses to report an UNRECOGNISED terminal status as completed', () => {
    for (const status of ['aborted', 'OK', '', 'partly-indexed']) {
      const client = createTestSocketClient();
      const { onRunFinish } = setup(client, { executionId: 'exec-1' });
      act(() => {
        sse.emit('index.ingest.completed', JSON.stringify({ status }));
      });
      expect(onRunFinish, `status ${JSON.stringify(status)} must not settle as completed`).toHaveBeenLastCalledWith('failed');
    }
  });

  it('reports execution.failed as a failed run', () => {
    const client = createTestSocketClient();
    const { onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.failed', JSON.stringify({ message: 'worker died' }));
    });

    expect(onRunFinish).toHaveBeenCalledWith('failed');
  });

  // `execution.failed`'s payload is `{code, safe_message, retryable}`
  // (`infra/db/repos/command_outbox.go:29-30`). The handler used to take NO
  // ARGUMENT, so all three were discarded and a cancellation, a deadline
  // retirement and a hard fault were indistinguishable on screen. Asserting on
  // the RENDERED text — not on a call count — is what makes this fail against
  // a handler that receives the frame and drops it anyway.
  it('surfaces the failure frame’s safe_message, code and retryability to the user', () => {
    const client = createTestSocketClient();
    const { getChatHistory, onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit(
        'execution.failed',
        JSON.stringify({ code: 'DEADLINE_EXCEEDED', safe_message: 'The execution deadline was exceeded.', retryable: true }),
      );
    });

    const rendered = getChatHistory().map((message) => String(message.content ?? '')).join('\n');
    expect(rendered).toContain('The execution deadline was exceeded.');
    expect(rendered).toContain('DEADLINE_EXCEEDED');
    expect(rendered).toMatch(/retried/i);
    expect(onRunFinish).toHaveBeenCalledWith('failed');
  });

  it('still says something useful when the failure frame carries no safe_message', () => {
    const client = createTestSocketClient();
    const { getChatHistory } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.failed', JSON.stringify({ code: 'CANCELLED', retryable: false }));
    });

    const rendered = getChatHistory().map((message) => String(message.content ?? '')).join('\n');
    expect(rendered).toContain('CANCELLED');
    expect(rendered).not.toMatch(/retried/i);
  });

  // `execution.replay_reset` is emitted when the durable log was pruned past
  // the cursor being resumed from (`infra/db/repos/replay_events.go:89-102`).
  // An SSE `event:` name with no registered listener is dropped SILENTLY by
  // EventSource, so before this the frame arrived and nothing happened: a
  // resumed long run rendered a transcript with an undisclosed hole in it.
  it('discloses a replay reset to the user WITHOUT ending the run', () => {
    const client = createTestSocketClient();
    const { getChatHistory, onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('execution.replay_reset', JSON.stringify({ reason: 'progress_retention_window_elapsed' }));
    });

    const rendered = getChatHistory().map((message) => String(message.content ?? '')).join('\n');
    expect(rendered, 'the gap notice must reach the transcript').toContain('progress_retention_window_elapsed');
    // The run is still going: a reset that settles the run would strand a
    // live execution in a terminal state and close its own stream.
    expect(onRunFinish).not.toHaveBeenCalled();
  });

  it('ignores every SSE frame while isAuthCheckSession is true', () => {
    const client = createTestSocketClient();
    const { onRunFinish, onMcpAuthRequired, setChatHistory } = setup(client, { executionId: 'exec-1', isAuthCheckSession: true });

    act(() => {
      sse.emit('execution.node_event', JSON.stringify({ type: 'mcp_authorization_required', message_id: 'm1' }));
      sse.emit('index.ingest.completed', JSON.stringify({ status: 'ok' }));
      sse.emit('execution.failed', JSON.stringify({ message: 'boom' }));
    });

    expect(setChatHistory).not.toHaveBeenCalled();
    expect(onMcpAuthRequired).not.toHaveBeenCalled();
    expect(onRunFinish).not.toHaveBeenCalled();
  });
  it('forwards a stream-open failure to onStreamError — the socket-fallback signal', () => {
    const client = createTestSocketClient();
    const { onStreamError, onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.fail();
    });

    expect(onStreamError).toHaveBeenCalledTimes(1);
    // A transport failure is NOT an execution failure: the run is about to
    // be re-dispatched on socket.io, so it must not be reported as failed.
    expect(onRunFinish).not.toHaveBeenCalled();
  });

  // issue #310: once the stream has genuinely opened, a LATER drop must not
  // re-dispatch the same run over socket.io — the execution is already
  // progressing server-side, so that would start a second run.
  it('does NOT forward a stream error to onStreamError once the stream has opened', () => {
    const client = createTestSocketClient();
    const { onStreamError, onRunFinish } = setup(client, { executionId: 'exec-1' });

    act(() => {
      sse.emit('open');
    });
    act(() => {
      sse.fail();
    });

    expect(onStreamError).not.toHaveBeenCalled();
    expect(onRunFinish).not.toHaveBeenCalled();
  });

  it('re-arms the open gate for a fresh executionId — a NEW run that never opens still falls back', () => {
    const client = createTestSocketClient();
    const onStreamError = vi.fn();
    const { rerender } = renderHook(
      (props: { executionId: string | undefined }) =>
        useToolkitChatSocket({
          isAuthCheckSession: false,
          onMcpAuthRequired: undefined,
          onRunFinish: vi.fn(),
          onStartTask: vi.fn(),
          setChatHistory: vi.fn(),
          activeConversationId: undefined,
          activeConversationUuid: undefined,
          projectId: 'proj-1',
          roomEnabled: false,
          executionId: props.executionId,
          onStreamError,
        }),
      { wrapper: withSocket(client), initialProps: { executionId: 'exec-1' } },
    );

    act(() => {
      sse.emit('open');
    });
    act(() => {
      sse.fail();
    });
    expect(onStreamError).not.toHaveBeenCalled();

    // A brand-new run gets its own stream and its own open-gate state.
    rerender({ executionId: 'exec-2' });
    act(() => {
      sse.fail();
    });
    expect(onStreamError).toHaveBeenCalledTimes(1);
  });

  // issue #310: "No message_id guard — any frame arriving on the stream
  // reaches the reducer, regardless of which run it belongs to."
  describe('message_id correlation', () => {
    it('routes every frame belonging to the SAME message_id', () => {
      const client = createTestSocketClient();
      const { setChatHistory } = setup(client, { executionId: 'exec-1' });

      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 't1' } }));
        sse.emit('execution.node_event', JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'hello' }));
      });

      expect(setChatHistory).toHaveBeenCalledTimes(2);
    });

    it('drops a frame naming a DIFFERENT message_id once one is tracked', () => {
      const client = createTestSocketClient();
      const { setChatHistory } = setup(client, { executionId: 'exec-1' });

      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 't1' } }));
      });
      expect(setChatHistory).toHaveBeenCalledTimes(1);

      act(() => {
        // A stray frame from a different run's message_id must not reach
        // the reducer and corrupt this run's transcript.
        sse.emit('execution.node_event', JSON.stringify({ type: 'chunk', message_id: 'stray-run', content: 'oops' }));
      });
      expect(setChatHistory).toHaveBeenCalledTimes(1);
    });

    it('still routes a frame that carries no message_id at all', () => {
      const client = createTestSocketClient();
      const { setChatHistory } = setup(client, { executionId: 'exec-1' });

      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 't1' } }));
        sse.emit('execution.node_event', JSON.stringify({ type: 'chunk', content: 'no id here' }));
      });

      expect(setChatHistory).toHaveBeenCalledTimes(2);
    });

    it('re-arms the tracked message_id for a fresh executionId', () => {
      const client = createTestSocketClient();
      const setChatHistory = vi.fn();
      const { rerender } = renderHook(
        (props: { executionId: string | undefined }) =>
          useToolkitChatSocket({
            isAuthCheckSession: false,
            onMcpAuthRequired: undefined,
            onRunFinish: vi.fn(),
            onStartTask: vi.fn(),
            setChatHistory,
            activeConversationId: undefined,
            activeConversationUuid: undefined,
            projectId: 'proj-1',
            roomEnabled: false,
            executionId: props.executionId,
            onStreamError: vi.fn(),
          }),
        { wrapper: withSocket(client), initialProps: { executionId: 'exec-1' } },
      );

      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm1', content: { task_id: 't1' } }));
      });
      expect(setChatHistory).toHaveBeenCalledTimes(1);

      // A brand-new run (its own executionId, its own message_id) must not
      // be gated by the PREVIOUS run's tracked message_id.
      rerender({ executionId: 'exec-2' });
      act(() => {
        sse.emit('execution.node_event', JSON.stringify({ type: 'start_task', message_id: 'm2', content: { task_id: 't2' } }));
      });
      expect(setChatHistory).toHaveBeenCalledTimes(2);
    });
  });
});

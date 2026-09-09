/**
 * `useChat` — the reducer that turns REST calls and SSE frames into the
 * transcript on screen (chat.hook.ts).
 *
 * The API is a hand-written double (`vi.fn`), not MSW: `useChat` never calls
 * `eliteaFetch` itself, it calls the `TChatAPI`/`TSupportApi` it is handed
 * through `ApiContext`. The stream half is real — `useSupportStream` runs for
 * true, driven through the `installTestEventSource` double — so a test that
 * sends a message and asserts the transcript exercises the actual wiring
 * between the two halves, not a stand-in for it.
 */
import type { ReactNode } from 'react';

import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { EXECUTION_EVENT_FAILED, EXECUTION_EVENT_NODE } from '@/shared/api/sse/executionEvents';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';

import { ApiContext } from './api.hook';
import { useChat } from './chat.hook';
import { SupportAssistantContextValue } from './supportContext.hook';
import type { TChatAPI, TConversationListItem, TRawConversation, TSupportAssistantContext } from '../types';
import type { TSupportApi, TSupportTurnStarted } from '../../api/adapter.api';

let registry: TestEventSourceRegistry;

function fakeConversation(uuid: string, overrides: Partial<TConversationListItem> = {}): TConversationListItem {
  return {
    id: 1,
    uuid,
    name: 'A conversation',
    is_private: true,
    author_id: 1,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    meta: {
      is_hidden: false,
      context_strategy: {
        name: 'default',
        enabled: false,
        created_at: '2026-01-01T00:00:00Z',
        last_optimized_at: null,
        max_context_tokens: 0,
        enable_summarization: false,
        summary_instructions: '',
        summary_llm_settings: null,
        preserve_recent_messages: 0,
        preserve_system_messages: false,
      },
      conversation_type: 'support',
    },
    source: 'widget',
    attachment_participant_id: null,
    instructions: null,
    participants_count: 1,
    message_groups_count: 0,
    users_count: 1,
    duration: 0,
    ...overrides,
  };
}

function fakeApi(overrides: Partial<TSupportApi> = {}): TSupportApi {
  return {
    getConversations: vi.fn().mockResolvedValue({ items: [], total: 0 }),
    getConversation: vi.fn().mockResolvedValue({ uuid: 'c1', message_groups: [] }),
    createConversation: vi.fn().mockResolvedValue(fakeConversation('created-1')),
    startTurn: vi.fn().mockResolvedValue({ events_url: '/stream/turn-1' } satisfies TSupportTurnStarted),
    uploadAttachment: vi
      .fn()
      .mockResolvedValue({ filepath: '/chat-attachments/created-1/notes.txt', file_size: 5 }),
    ...overrides,
  } as TSupportApi;
}

function wrapperWith(api: TChatAPI, context: TSupportAssistantContext | null = null) {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return (
      <ApiContext.Provider value={api as TSupportApi}>
        <SupportAssistantContextValue.Provider value={context}>{children}</SupportAssistantContextValue.Provider>
      </ApiContext.Provider>
    );
  };
}

const BASE_PROPS = {
  welcomeMessage: 'Hi! Ask me anything.',
  supportProjectId: null,
  initialHistory: [] as TConversationListItem[],
  initialConversation: null as TRawConversation | null,
  isInitLoading: false,
};

beforeEach(() => {
  registry = installTestEventSource();
  resetConfigForTests();
});

afterEach(() => {
  registry.restore();
});

describe('useChat — initial transcript', () => {
  it('shows the welcome message when there is no prior conversation', () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    expect(result.current.messages).toEqual([
      expect.objectContaining({ role: 'assistant', content: 'Hi! Ask me anything.' }),
    ]);
    expect(result.current.currentConversationId).toBe('');
  });

  it('shows NO messages when the operator configured an empty welcome message', () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat({ ...BASE_PROPS, welcomeMessage: '' }), {
      wrapper: wrapperWith(api),
    });

    expect(result.current.messages).toEqual([]);
  });

  it('restores the most recent conversation\'s transcript when one exists', () => {
    const api = fakeApi();
    const conversation: TRawConversation = {
      uuid: 'c1',
      message_groups: [
        { uuid: 'g1', sent_to: null, message_items: [{ item_type: 'text_message', item_details: { content: 'Earlier answer' } }] },
      ],
    };
    const { result } = renderHook(
      () =>
        useChat({
          ...BASE_PROPS,
          initialHistory: [fakeConversation('c1')],
          initialConversation: conversation,
        }),
      { wrapper: wrapperWith(api) },
    );

    expect(result.current.messages).toEqual([
      expect.objectContaining({ role: 'assistant', content: 'Earlier answer' }),
    ]);
    expect(result.current.currentConversationId).toBe('c1');
    expect(result.current.history).toEqual([fakeConversation('c1')]);
  });

  it('falls back to the welcome message when the most recent conversation parses to nothing', () => {
    const api = fakeApi();
    const conversation: TRawConversation = { uuid: 'c1', message_groups: [] };
    const { result } = renderHook(
      () =>
        useChat({ ...BASE_PROPS, initialHistory: [fakeConversation('c1')], initialConversation: conversation }),
      { wrapper: wrapperWith(api) },
    );

    expect(result.current.messages).toEqual([expect.objectContaining({ role: 'assistant' })]);
  });

  it('is LOADING while the init gate has not resolved, regardless of switching state', () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat({ ...BASE_PROPS, isInitLoading: true }), {
      wrapper: wrapperWith(api),
    });

    expect(result.current.isLoading).toBe(true);
  });
});

describe('useChat — sending the first message', () => {
  it('creates a conversation, then starts the turn and opens the stream', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('How do I reset my password?');
    });

    expect(api.createConversation).toHaveBeenCalledOnce();
    expect(api.startTurn).toHaveBeenCalledWith(
      'created-1',
      expect.objectContaining({ content: 'How do I reset my password?' }),
    );
    expect(result.current.currentConversationId).toBe('created-1');
    expect(result.current.messages.at(-1)).toEqual(
      expect.objectContaining({ role: 'user', content: 'How do I reset my password?' }),
    );
    expect(registry.getSources().at(-1)?.url).toBe('/stream/turn-1');
  });

  it('sends the page context alongside the question', async () => {
    const api = fakeApi();
    const context: TSupportAssistantContext = { current_page: '/agents', project_id: 7 };
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api, context) });

    await act(async () => {
      await result.current.handleSend('Where is the agent list?');
    });

    expect(api.startTurn).toHaveBeenCalledWith(
      'created-1',
      expect.objectContaining({ support_assistant_context: context }),
    );
  });

  it('reports a create failure as an assistant error message, and starts no turn', async () => {
    const api = fakeApi({ createConversation: vi.fn().mockRejectedValue(new Error('network down')) });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('Hello?');
    });

    expect(result.current.messages.at(-1)).toEqual(
      expect.objectContaining({ isError: true, content: 'Failed to create conversation. Please try again.' }),
    );
    expect(api.startTurn).not.toHaveBeenCalled();
  });

  it('reports a start-turn failure as an assistant error message', async () => {
    const api = fakeApi({ startTurn: vi.fn().mockRejectedValue(new Error('500')) });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('Hello?');
    });

    expect(result.current.messages.at(-1)).toEqual(
      expect.objectContaining({ isError: true, content: 'Failed to reach the support assistant. Please try again.' }),
    );
  });

  it('reports a 200 with no events_url rather than spinning forever', async () => {
    const api = fakeApi({ startTurn: vi.fn().mockResolvedValue({} satisfies TSupportTurnStarted) });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('Hello?');
    });

    expect(result.current.messages.at(-1)).toEqual(
      expect.objectContaining({
        isError: true,
        content: 'The support assistant did not return a response stream.',
      }),
    );
  });

  it('reuses the CURRENT conversation on a second message, without creating another', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('First question');
    });
    await act(async () => {
      await result.current.handleSend('Second question');
    });

    expect(api.createConversation).toHaveBeenCalledOnce();
    expect(api.startTurn).toHaveBeenCalledTimes(2);
  });
});

// Issue #625 item 2 (single file), extended by #877 (multi-file): the
// attach control's send-time upload.
describe('useChat — sending an attachment', () => {
  it('uploads the file, then starts the turn naming it', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    const file = new File(['diagnostic log'], 'notes.txt', { type: 'text/plain' });

    await act(async () => {
      await result.current.handleSend('What does this mean?', [file]);
    });

    expect(api.uploadAttachment).toHaveBeenCalledWith('created-1', file);
    expect(api.startTurn).toHaveBeenCalledWith(
      'created-1',
      expect.objectContaining({
        attachments: [{ filepath: '/chat-attachments/created-1/notes.txt', name: 'notes.txt' }],
      }),
    );
  });

  it('uploads every attached file in order, then starts the turn naming all of them', async () => {
    const api = fakeApi({
      uploadAttachment: vi
        .fn()
        .mockResolvedValueOnce({ filepath: '/chat-attachments/created-1/notes.txt', file_size: 5 })
        .mockResolvedValueOnce({ filepath: '/chat-attachments/created-1/diagram.png', file_size: 9 }),
    });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    const fileA = new File(['diagnostic log'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['a diagram'], 'diagram.png', { type: 'image/png' });
    const statuses: Array<[string, string]> = [];

    await act(async () => {
      await result.current.handleSend('What does this mean?', [fileA, fileB], (file, status) =>
        statuses.push([file.name, status]),
      );
    });

    expect(api.uploadAttachment).toHaveBeenNthCalledWith(1, 'created-1', fileA);
    expect(api.uploadAttachment).toHaveBeenNthCalledWith(2, 'created-1', fileB);
    expect(api.startTurn).toHaveBeenCalledWith(
      'created-1',
      expect.objectContaining({
        attachments: [
          { filepath: '/chat-attachments/created-1/notes.txt', name: 'notes.txt' },
          { filepath: '/chat-attachments/created-1/diagram.png', name: 'diagram.png' },
        ],
      }),
    );
    expect(statuses).toEqual([
      ['notes.txt', 'uploading'],
      ['notes.txt', 'done'],
      ['diagram.png', 'uploading'],
      ['diagram.png', 'done'],
    ]);
  });

  it('sends no attachments field when no file is given (the ordinary case)', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('Just a question');
    });

    expect(api.uploadAttachment).not.toHaveBeenCalled();
    expect(api.startTurn).toHaveBeenCalledWith(
      'created-1',
      expect.objectContaining({ attachments: undefined }),
    );
  });

  // A FAILED UPLOAD REPORTS FAILURE RATHER THAN A FABRICATED SUCCESS: the
  // turn must not start believing it carried a file that never landed, and
  // no LATER file in the same send is uploaded either.
  it('reports an upload failure and starts NO turn, stopping before later files', async () => {
    const api = fakeApi({ uploadAttachment: vi.fn().mockRejectedValue(new Error('507')) });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    const fileA = new File(['x'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['y'], 'diagram.png', { type: 'image/png' });

    await act(async () => {
      await result.current.handleSend('See attached', [fileA, fileB]);
    });

    expect(result.current.messages.at(-1)).toEqual(
      expect.objectContaining({ isError: true, content: 'Failed to attach the file. Please try again.' }),
    );
    expect(api.uploadAttachment).toHaveBeenCalledOnce();
    expect(api.startTurn).not.toHaveBeenCalled();
  });
});

describe('useChat — streamed frames', () => {
  async function openTurn(api: TSupportApi = fakeApi()) {
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });
    return { result };
  }

  it('marks a "start_task" frame as the streaming placeholder, with a status message', async () => {
    const { result } = await openTurn();

    act(() => {
      registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'start_task', message_id: 'm1' }),
      );
    });

    const assistantMessage = result.current.messages.find((m) => m.id === 'm1');
    expect(assistantMessage).toEqual(
      expect.objectContaining({ role: 'assistant', isStreaming: true, statusMessage: 'Starting up...' }),
    );
  });

  /*
   * THE FRAME SEQUENCE THIS PLATFORM ACTUALLY SENDS, and the defect it found.
   *
   * Measured on the live standalone stack, one support turn streams:
   * `agent_start`, `agent_on_transitional_edge`, `agent_llm_start`, then a long
   * run of `agent_llm_chunk` that carries the answer text. There is NO
   * `start_task`, and there is no `chunk` frame at all.
   *
   * The reducer opened its bubble only on `start_task` and read content only
   * from `chunk`. So every frame above matched nothing, the panel stayed empty,
   * and the answer was visible only in the database. Nothing caught it: the
   * unit tests all opened their turn with `start_task`, and no E2E journey
   * existed.
   */
  it('renders an answer that arrives with no start_task and only agent_llm_chunk', async () => {
    const { result } = await openTurn();

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_start', message_id: 'm9' })));
    expect(result.current.messages.find((m) => m.id === 'm9')).toEqual(
      expect.objectContaining({ role: 'assistant', isStreaming: true }),
    );

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_llm_start', message_id: 'm9' })));
    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'agent_llm_chunk', message_id: 'm9', content: 'SUPPORT-' }),
      ),
    );
    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'agent_llm_chunk', message_id: 'm9', content: 'BOT-OK' }),
      ),
    );

    expect(result.current.messages.find((m) => m.id === 'm9')?.content).toBe('SUPPORT-BOT-OK');
  });

  // A chunk with no content is a progress ping, so it keeps the status label
  // rather than clearing it and rendering an empty bubble.
  it('keeps the status label for a chunk that carries no content', async () => {
    const { result } = await openTurn();

    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'agent_llm_chunk', message_id: 'm10', content: null }),
      ),
    );

    const message = result.current.messages.find((m) => m.id === 'm10');
    expect(message?.content).toBe('');
    expect(message?.statusMessage).toBe('Writing response...');
  });

  // The FIRST frame opens the bubble; a later opening frame must not open a
  // second one for the same turn.
  it('opens exactly one bubble per turn', async () => {
    const { result } = await openTurn();

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_start', message_id: 'm11' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_llm_start', message_id: 'm11' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm11' })));

    expect(result.current.messages.filter((m) => m.id === 'm11')).toHaveLength(1);
  });

  it('walks the status through agent_llm_start and agent_tool_start', async () => {
    const { result } = await openTurn();

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_llm_start', message_id: 'm1' })));
    expect(result.current.messages.find((m) => m.id === 'm1')?.statusMessage).toBe('Looking things up...');

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_tool_start', message_id: 'm1' })));
    expect(result.current.messages.find((m) => m.id === 'm1')?.statusMessage).toBe('Consulting knowledge base...');
  });

  it('ignores the bookkeeping-only frame types', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    const before = result.current.messages.find((m) => m.id === 'm1');

    for (const type of ['agent_tool_end', 'agent_llm_end', 'agent_on_transitional_edge', 'agent_on_function_tool_node']) {
      act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type, message_id: 'm1' })));
    }

    expect(result.current.messages.find((m) => m.id === 'm1')).toEqual(before);
  });

  it('accumulates chunk content and clears the status message', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'Hel' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'lo' })));

    const message = result.current.messages.find((m) => m.id === 'm1');
    expect(message?.content).toBe('Hello');
    expect(message?.statusMessage).toBeUndefined();
    expect(message?.isStreaming).toBe(true);
  });

  it('stringifies a non-string chunk payload', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'AIMessageChunk', message_id: 'm1', content: { note: 'structured' } }),
      ),
    );

    expect(result.current.messages.find((m) => m.id === 'm1')?.content).toBe('{"note":"structured"}');
  });

  it('stops streaming when a chunk carries a finish_reason', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({
          type: 'chunk',
          message_id: 'm1',
          content: 'Done.',
          response_metadata: { finish_reason: 'stop' },
        }),
      ),
    );

    expect(result.current.messages.find((m) => m.id === 'm1')?.isStreaming).toBe(false);
  });

  it('only updates agent_llm_chunk\'s status message when it actually changes', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_llm_chunk', message_id: 'm1' })));

    expect(result.current.messages.find((m) => m.id === 'm1')?.statusMessage).toBe('Writing response...');
  });

  it('finishes on agent_response, ready to animate', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() =>
      void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_response', message_id: 'm1', content: 'Final answer' })),
    );

    expect(result.current.messages.find((m) => m.id === 'm1')).toEqual(
      expect.objectContaining({ content: 'Final answer', isStreaming: false, isAnimating: true }),
    );
    // The stream itself detaches on this frame too (stream.hook.ts).
    expect(registry.getSources().at(-1)?.closed).toBe(true);
  });

  it('stringifies a non-string agent_response payload', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() =>
      void registry.emit(
        EXECUTION_EVENT_NODE,
        JSON.stringify({ type: 'agent_response', message_id: 'm1', content: { answer: 42 } }),
      ),
    );

    expect(result.current.messages.find((m) => m.id === 'm1')?.content).toBe('{"answer":42}');
  });

  it('finishes on pipeline_finish without animating', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'partial' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'pipeline_finish', message_id: 'm1' })));

    expect(result.current.messages.find((m) => m.id === 'm1')).toEqual(
      expect.objectContaining({ content: 'partial', isStreaming: false }),
    );
  });

  it('marks a server "error" frame as an error message, string content', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'error', message_id: 'm1', content: 'Budget exceeded' })));

    expect(result.current.messages.find((m) => m.id === 'm1')).toEqual(
      expect.objectContaining({ isError: true, isStreaming: false, isAnimating: false, content: 'Budget exceeded' }),
    );
  });

  it('falls back to a generic message for an "agent_exception" with no string content', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_exception', message_id: 'm1', content: null })));

    expect(result.current.messages.find((m) => m.id === 'm1')?.content).toBe('An error occurred');
  });

  it('completes the typewriter animation via handleAnimationComplete', async () => {
    const { result } = await openTurn();
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_response', message_id: 'm1', content: 'Done' })));
    expect(result.current.messages.find((m) => m.id === 'm1')?.isAnimating).toBe(true);

    act(() => result.current.handleAnimationComplete('m1'));

    expect(result.current.messages.find((m) => m.id === 'm1')?.isAnimating).toBe(false);
  });
});

describe('useChat — a dropped stream settles the placeholder', () => {
  it('turns an EMPTY streaming placeholder into the failure reason', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({ safe_message: 'Connection lost' })));

    const message = result.current.messages.find((m) => m.id === 'm1');
    expect(message).toEqual(expect.objectContaining({ isStreaming: false, isError: true, content: 'Connection lost' }));
  });

  it('leaves PARTIAL content alone — only clears the streaming flag', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk', message_id: 'm1', content: 'So far so' })));
    act(() => void registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({ safe_message: 'Connection lost' })));

    const message = result.current.messages.find((m) => m.id === 'm1');
    expect(message?.content).toBe('So far so');
    expect(message?.isStreaming).toBe(false);
    expect(message?.isError).toBeUndefined();
  });
});

describe('useChat — new chat and conversation switching', () => {
  it('handleNewChat drops the stream, resets the transcript and clears the draft', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });
    act(() => result.current.setInputText('unsent draft'));

    act(() => result.current.handleNewChat());

    expect(result.current.currentConversationId).toBe('');
    expect(result.current.inputText).toBe('');
    expect(result.current.messages).toEqual([expect.objectContaining({ role: 'assistant' })]);
    expect(registry.getSources().at(-1)?.closed).toBe(true);
  });

  it('handleSelectConversation is a no-op when the target IS the current one', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });
    const before = result.current.messages;

    await act(async () => {
      await result.current.handleSelectConversation('created-1');
    });

    expect(api.getConversation).not.toHaveBeenCalled();
    expect(result.current.messages).toBe(before);
  });

  it('loads a DIFFERENT conversation\'s transcript, closing the current stream first', async () => {
    const api = fakeApi({
      getConversation: vi.fn().mockResolvedValue({
        uuid: 'c2',
        message_groups: [
          { uuid: 'g1', sent_to: 5, message_items: [{ item_type: 'text_message', content: 'Earlier question' }] },
        ],
      }),
    });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });
    await act(async () => {
      await result.current.handleSend('Question');
    });

    await act(async () => {
      await result.current.handleSelectConversation('c2');
    });

    expect(result.current.currentConversationId).toBe('c2');
    expect(result.current.messages).toEqual([expect.objectContaining({ role: 'user', content: 'Earlier question' })]);
    expect(result.current.isLoading).toBe(false);
    // Selecting a different conversation closes the turn's stream rather than
    // leaving it attached to a transcript no longer on screen.
    expect(registry.getSources().at(-1)?.closed).toBe(true);
  });

  it('falls back to the welcome message when loading the conversation FAILS', async () => {
    const api = fakeApi({ getConversation: vi.fn().mockRejectedValue(new Error('gone')) });
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSelectConversation('missing-conversation');
    });

    expect(result.current.messages).toEqual([expect.objectContaining({ role: 'assistant' })]);
    expect(result.current.isLoading).toBe(false);
  });
});

describe('useChat — derived streaming flag', () => {
  it('is true while any message is streaming or animating, false once settled', async () => {
    const api = fakeApi();
    const { result } = renderHook(() => useChat(BASE_PROPS), { wrapper: wrapperWith(api) });

    await act(async () => {
      await result.current.handleSend('Question');
    });
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task', message_id: 'm1' })));
    expect(result.current.isStreaming).toBe(true);

    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'pipeline_finish', message_id: 'm1' })));
    await waitFor(() => {
      expect(result.current.isStreaming).toBe(false);
    });
  });
});

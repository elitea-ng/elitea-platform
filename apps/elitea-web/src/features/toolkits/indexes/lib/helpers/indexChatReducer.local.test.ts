import { describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '../../../../../test/webstorage';

import { IndexStatuses } from '../constants/indexDetails.constants';

import type { IndexChatMessage } from './indexChat.helpers';
import { generateChatMessageBasedOnResponse } from './indexChatReducer.local';

// `generateChatMessageBasedOnResponse`'s error-handling paths call
// `notifyTaskError` (`soundNotification.local.ts`), which reads its sound
// preference via `shared/lib/storage.ts`'s `createStorage('local')`. Node's
// own `localStorage` global shadows jsdom's and is `undefined` without
// `--localstorage-file` (see `webstorage.ts`'s own doc comment) — this
// installs the in-memory shim so those code paths don't throw.
installWebStorageShim();

describe('generateChatMessageBasedOnResponse', () => {
  const onFinish = vi.fn();

  it('start_task appends a loading assistant message and calls onStartTask', () => {
    const onStartTask = vi.fn();
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'start_task', content: { task_id: 'task-1' } },
      chatHistory: [],
      onFinish,
      onStartTask,
    });
    expect(result).toHaveLength(1);
    expect(result[0]?.isLoading).toBe(true);
    expect(result[0]?.task_id).toBe('task-1');
    expect(onStartTask).toHaveBeenCalledWith('task-1');
  });

  it('agent_tool_start adds a processing tool action to the matching message', () => {
    const history: IndexChatMessage[] = [
      { id: 'm1', role: 'assistant', content: '', created_at: 1, participant_id: 'system', toolActions: [] },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: {
        message_id: 'm1',
        type: 'agent_tool_start',
        response_metadata: { tool_run_id: 'run-1', tool_name: 'search_index' },
      },
      chatHistory: history,
      onFinish,
    });
    expect(result[0]?.toolActions).toHaveLength(1);
    expect(result[0]?.toolActions?.[0]).toMatchObject({ id: 'run-1', name: 'search_index', status: 'processing' });
  });

  it('agent_tool_start does not duplicate an existing tool action for the same tool_run_id', () => {
    const history: IndexChatMessage[] = [
      {
        id: 'm1',
        role: 'assistant',
        content: '',
        created_at: 1,
        participant_id: 'system',
        toolActions: [{ id: 'run-1', status: 'processing', created_at: 1, type: 'tool' }],
      },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'agent_tool_start', response_metadata: { tool_run_id: 'run-1' } },
      chatHistory: history,
      onFinish,
    });
    expect(result[0]?.toolActions).toHaveLength(1);
  });

  it('a repeated agent_tool_start of a running step renames it, and never reopens a finished one', () => {
    const history: IndexChatMessage[] = [
      {
        id: 'm1',
        role: 'assistant',
        content: '',
        created_at: 1,
        participant_id: 'system',
        toolActions: [
          { id: 'write', name: 'Indexing documents', status: 'processing', created_at: 1, type: 'tool' },
          { id: 'done', name: 'Planning', status: 'complete', created_at: 1, type: 'tool' },
        ],
      },
    ];
    const start = (id: string, name: string) =>
      generateChatMessageBasedOnResponse({
        message: { message_id: 'm1', type: 'agent_tool_start', response_metadata: { tool_run_id: id, tool_name: name } },
        chatHistory: history,
        onFinish,
      });
    start('write', 'Indexing documents: 10 of 30 (40 chunks)');
    start('done', 'Renamed');
    expect(history[0]?.toolActions).toHaveLength(2);
    expect(history[0]?.toolActions?.[0]?.name).toBe('Indexing documents: 10 of 30 (40 chunks)');
    expect(history[0]?.toolActions?.[1]?.name).toBe('Planning');
  });

  it('agent_tool_end marks the matching action complete and records outputs', () => {
    const history: IndexChatMessage[] = [
      {
        id: 'm1',
        role: 'assistant',
        content: '',
        created_at: 1,
        participant_id: 'system',
        toolActions: [{ id: 'run-1', status: 'processing', created_at: 0, type: 'tool' }],
      },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: {
        message_id: 'm1',
        type: 'agent_tool_end',
        content: 'done',
        response_metadata: { tool_run_id: 'run-1', tool_output: { ok: true }, timestamp_finish: 1000 },
      },
      chatHistory: history,
      onFinish,
    });
    const action = result[0]?.toolActions?.[0];
    expect(action?.status).toBe('complete');
    expect(action?.toolOutputs).toEqual({ ok: true });
  });

  it('chunk updates message content and, on finish_reason, marks streaming complete + prepends tool summary', () => {
    const finish = vi.fn();
    const history: IndexChatMessage[] = [
      {
        id: 'm1',
        role: 'assistant',
        content: '',
        created_at: 1,
        participant_id: 'system',
        isLoading: true,
        isStreaming: true,
        toolActions: [{ id: 'run-1', name: 'search_index', status: 'complete', created_at: 0, ended_at: 1, type: 'tool' }],
      },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'chunk', content: 'final answer', response_metadata: { finish_reason: 'stop' } },
      chatHistory: history,
      onFinish: finish,
    });
    expect(result[0]?.isStreaming).toBe(false);
    expect(result[0]?.isLoading).toBe(false);
    expect(result[0]?.content).toContain('final answer');
    expect(result[0]?.content).toContain('search_index');
    expect(finish).toHaveBeenCalledWith(IndexStatuses.success);
  });

  it('chunk with an empty-string content falls back to response_metadata.message (|| not ??, matches baseline)', () => {
    const history: IndexChatMessage[] = [
      { id: 'm1', role: 'assistant', content: '', created_at: 1, participant_id: 'system', isLoading: true, isStreaming: true },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'chunk', content: '', response_metadata: { message: 'fallback text' } },
      chatHistory: history,
      onFinish,
    });
    // Pre-fix (`??`): '' is neither null nor undefined, so it would be kept
    // as-is and this would be '' instead of the fallback text.
    expect(result[0]?.content).toBe('fallback text');
  });

  it('agent_tool_error marks the action errored and finishes with fail', () => {
    const finish = vi.fn();
    const history: IndexChatMessage[] = [
      {
        id: 'm1',
        role: 'assistant',
        content: '',
        created_at: 1,
        participant_id: 'system',
        toolActions: [{ id: 'run-1', status: 'processing', created_at: 0, type: 'tool' }],
      },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'agent_tool_error', content: 'boom', response_metadata: { tool_run_id: 'run-1' } },
      chatHistory: history,
      onFinish: finish,
    });
    expect(result[0]?.toolActions?.[0]?.status).toBe('error');
    expect(finish).toHaveBeenCalledWith(IndexStatuses.fail);
  });

  it('error on an existing message marks it failed with the exception set', () => {
    const finish = vi.fn();
    const history: IndexChatMessage[] = [
      { id: 'm1', role: 'assistant', content: '', created_at: 1, participant_id: 'system', isLoading: true },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'error', content: 'bad thing' },
      chatHistory: history,
      onFinish: finish,
    });
    expect(result[0]?.exception).toBe('bad thing');
    expect(finish).toHaveBeenCalledWith(IndexStatuses.fail);
  });

  it('error with no matching message appends a new error message instead', () => {
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'does-not-exist', type: 'error', content: 'bad thing' },
      chatHistory: [],
      onFinish: vi.fn(),
    });
    expect(result).toHaveLength(1);
    expect(result[0]?.content).toContain('Error occurred during tool testing');
  });

  it('an unknown message type is a no-op', () => {
    const history: IndexChatMessage[] = [
      { id: 'm1', role: 'assistant', content: 'unchanged', created_at: 1, participant_id: 'system' },
    ];
    const result = generateChatMessageBasedOnResponse({
      message: { message_id: 'm1', type: 'something_else' },
      chatHistory: history,
      onFinish: vi.fn(),
    });
    expect(result).toEqual(history);
  });
});

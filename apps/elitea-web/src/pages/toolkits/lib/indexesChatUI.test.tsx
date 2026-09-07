/*
 * `INDEXES_CHAT_UI` — the three cross-slice components the index panel's chat
 * is injected with, and the row translation between them.
 *
 * It had no test. The interesting half is `toChatMessage`, which reconciles a
 * union the baseline never reconciled: a live socket turn carries snake_case
 * `created_at`/`participant_id` and a recovered conversation carries camelCase
 * `createdAt`/`participantId`, and BOTH arrive on the same list. A branch read
 * the wrong way here does not throw — it renders a message with an empty
 * timestamp, or with an author label the app invented.
 *
 * The bridge components are rendered rather than called, because the row
 * translation runs inside a `useMemo`.
 */
import { render } from '@testing-library/react';
import type { ReactElement } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { INDEXES_CHAT_UI } from './indexesChatUI';

interface ChatMessageListProps {
  readonly chatHistory: readonly Record<string, unknown>[];
  readonly isStreaming: boolean;
  readonly messageActions: { readonly onCopyToClipboard: (message: { id: string }) => void };
  readonly pagination: { readonly isLoadingMore: boolean };
}

/**
 * The props the bridge hands the REAL widget.
 *
 * Read off the element the bridge returns rather than out of a rendered DOM:
 * the subject is the translation, and mounting `ChatMessageList` would make
 * this a test of that widget's markup instead.
 */
function chatMessageListProps(
  rows: readonly unknown[],
  overrides: Record<string, unknown> = {},
): ChatMessageListProps {
  const captured: { current: ChatMessageListProps | undefined } = { current: undefined };
  const Bridge = INDEXES_CHAT_UI.ChatMessageList as unknown as (props: Record<string, unknown>) => ReactElement;

  function Probe() {
    const element = Bridge({
      chat_history: rows,
      activeConversation: undefined,
      isLoading: false,
      isStreaming: false,
      isLoadingMore: false,
      interaction_uuid: 'uuid-1',
      onCopyToClipboard: vi.fn(),
      ...overrides,
    });
    captured.current = element.props as ChatMessageListProps;
    return null;
  }

  render(<Probe />);
  return captured.current as ChatMessageListProps;
}

describe('INDEXES_CHAT_UI.ChatMessageList', () => {
  it('reads a recovered conversation\'s camelCase fields', () => {
    const props = chatMessageListProps([
      {
        id: 'm-1',
        role: 'user',
        name: 'Alice',
        content: 'hello',
        createdAt: '2026-09-06T12:00:00.000Z',
        participantId: 'p-1',
        taskId: 't-1',
      },
    ]);
    expect(props.chatHistory[0]).toEqual({
      id: 'm-1',
      role: 'user',
      name: 'Alice',
      content: 'hello',
      createdAt: '2026-09-06T12:00:00.000Z',
      participantId: 'p-1',
      taskId: 't-1',
    });
  });

  it('reads a live turn\'s snake_case fields, and its epoch timestamp', () => {
    const epoch = Date.UTC(2026, 8, 6, 12, 0, 0);
    const props = chatMessageListProps([
      { id: 'm-2', content: 'streaming', created_at: epoch, participant_id: 'p-2', task_id: 't-2', isStreaming: true },
    ]);
    expect(props.chatHistory[0]).toEqual({
      id: 'm-2',
      // A row with no role is an assistant turn — the default the index panel
      // has always rendered.
      role: 'assistant',
      // `''`, not an invented label: ChatMessageList resolves the participant
      // name itself when the row carries none.
      name: '',
      content: 'streaming',
      createdAt: new Date(epoch).toISOString(),
      participantId: 'p-2',
      taskId: 't-2',
      isStreaming: true,
    });
  });

  it('accepts a snake_case timestamp that is already a string', () => {
    const props = chatMessageListProps([{ id: 'm-3', created_at: '2026-09-06T12:00:00.000Z' }]);
    expect(props.chatHistory[0]?.['createdAt']).toBe('2026-09-06T12:00:00.000Z');
  });

  it('never fabricates a timestamp for a row that carries none', () => {
    const props = chatMessageListProps([{ id: 'm-4' }]);
    expect(props.chatHistory[0]?.['createdAt']).toBe('');
  });

  it('omits an absent optional field rather than writing undefined into it', () => {
    // exactOptionalPropertyTypes: `{participantId: undefined}` and "no
    // participantId" are different values to every consumer downstream.
    const message = chatMessageListProps([{ id: 'm-5' }]).chatHistory[0] as Record<string, unknown>;
    for (const key of ['participantId', 'taskId', 'toolActions', 'exception', 'isStreaming', 'isLoading']) {
      expect(Object.hasOwn(message, key), `${key} was written as undefined`).toBe(false);
    }
  });

  it('bridges the copy action from a whole message back to an id', () => {
    const onCopyToClipboard = vi.fn();
    const props = chatMessageListProps([{ id: 'm-6' }], { onCopyToClipboard });
    props.messageActions.onCopyToClipboard({ id: 'm-6' });
    expect(onCopyToClipboard).toHaveBeenCalledWith('m-6');
  });

  it('carries the streaming and paging flags to the widget', () => {
    const props = chatMessageListProps([], { isStreaming: true, isLoadingMore: true });
    expect(props.isStreaming).toBe(true);
    expect(props.pagination.isLoadingMore).toBe(true);
  });
});

describe('INDEXES_CHAT_UI.LLMModelSelector', () => {
  interface SelectorProps {
    readonly selectedModel: unknown;
    readonly models: readonly unknown[];
    readonly llmSettings: Record<string, unknown>;
    readonly onSelectModel: (model: unknown) => void;
    readonly onSetLLMSettings: (settings: unknown) => void;
  }

  function selectorProps(overrides: Record<string, unknown> = {}): SelectorProps {
    const Bridge = INDEXES_CHAT_UI.LLMModelSelector as unknown as (props: Record<string, unknown>) => ReactElement;
    return Bridge({
      selectedModel: undefined,
      onSelectModel: vi.fn(),
      models: [],
      llmSettings: undefined,
      onSetLLMSettings: vi.fn(),
      ...overrides,
    }).props as SelectorProps;
  }

  it('turns an absent selection into null and absent settings into an empty object', () => {
    const props = selectorProps();
    // `null`, not `undefined`: the widget's own prop is nullable, and
    // `undefined` there reads as "not supplied" and re-enables its default.
    expect(props.selectedModel).toBeNull();
    expect(props.llmSettings).toEqual({});
    expect(props.models).toEqual([]);
  });

  it('carries a real selection, model list and settings through', () => {
    const model = { id: 'gpt', name: 'GPT' };
    const settings = { temperature: 0.5 };
    const props = selectorProps({ selectedModel: model, models: [model], llmSettings: settings });
    expect(props.selectedModel).toBe(model);
    expect(props.models).toEqual([model]);
    expect(props.llmSettings).toBe(settings);
  });

  it('forwards both callbacks', () => {
    const onSelectModel = vi.fn();
    const onSetLLMSettings = vi.fn();
    const props = selectorProps({ onSelectModel, onSetLLMSettings });
    props.onSelectModel({ id: 'gpt' });
    props.onSetLLMSettings({ temperature: 1 });
    expect(onSelectModel).toHaveBeenCalledWith({ id: 'gpt' });
    expect(onSetLLMSettings).toHaveBeenCalledWith({ temperature: 1 });
  });
});

describe('INDEXES_CHAT_UI.ClearChatButton', () => {
  it('forwards its one callback', () => {
    const onClear = vi.fn();
    const Bridge = INDEXES_CHAT_UI.ClearChatButton as unknown as (props: { onClear: () => void }) => ReactElement;
    const props = Bridge({ onClear }).props as { onClear: () => void };
    expect(props.onClear).toBe(onClear);
  });
});

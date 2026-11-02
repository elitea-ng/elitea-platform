/**
 * The three cross-slice components `features/toolkits`' `IndexesTab` injects
 * into `IndexesContainer` — supplied here because `pages/` is the lowest
 * layer that may legally import all of them at once:
 *
 *   `widgets/llm-model-selector` → `LLMModelSelector`
 *   `features/chat-messages`     → `ChatMessageList`
 *   `widgets/chat`               → `ClearChatButton`
 *
 * A `features/` file may import none of them (`no-upward-from-features` for
 * the two widgets, `no-sideways-features` for chat-messages); `pages/` may
 * import all three, through their barrels.
 *
 * Both adapters below are shape bridges only — no behaviour is invented.
 * They exist because the `indexes` slice declared its DI contract against
 * the BASELINE's prop names (`ui/IndexDetails/IndexChat.tsx`'s
 * `LLMModelSelectorProps`/`ChatMessageListProps`, ported verbatim from
 * `IndexChat.jsx`) while the components that landed here were ported with
 * this app's own naming. Bridging at the single injection point is strictly
 * better than editing either contract: the slice keeps a prop shape its own
 * tests assert, and the widgets keep the shape their other callers use.
 */
import type { ComponentProps, ReactNode } from 'react';
import { useMemo } from 'react';

import { ChatMessageList as RealChatMessageList } from '@/features/chat-messages';
import type { ChatMessage } from '@/features/chat-messages';
import type { IndexesTabChatUI } from '@/features/toolkits';
import { ClearChatButton as RealClearChatButton } from '@/widgets/chat';
import { LLMModelSelector as RealLLMModelSelector } from '@/widgets/llm-model-selector';

/**
 * A row as it arrives from the index chat: either an `IndexChatMessage`
 * (live socket/SSE turn — snake_case `created_at`/`participant_id`) or an
 * `entities/message` `Message` (recovered conversation — camelCase
 * `createdAt`/`participantId`). `useToolkitChat.types.ts`'s own
 * `ToolkitChatMessage` doc comment records that this union is genuinely
 * mixed at runtime and was never reconciled in the baseline either; this
 * function is where it finally is, for rendering purposes only.
 */
interface MixedChatRow {
  readonly id?: string;
  readonly role?: string;
  readonly name?: string;
  readonly content?: string;
  readonly created_at?: number | string;
  readonly createdAt?: string;
  readonly participant_id?: string;
  readonly participantId?: string;
  readonly task_id?: string;
  readonly taskId?: string;
  readonly toolActions?: readonly unknown[];
  readonly exception?: unknown;
  readonly isStreaming?: boolean;
  readonly isLoading?: boolean;
  readonly [key: string]: unknown;
}

/** `IndexChatMessage.created_at` is an epoch number; `ChatMessage.createdAt` is a string. Never fabricate a timestamp for a row that carries none. */
function resolveCreatedAt(row: MixedChatRow): string {
  if (typeof row.createdAt === 'string') return row.createdAt;
  if (typeof row.created_at === 'number') return new Date(row.created_at).toISOString();
  if (typeof row.created_at === 'string') return row.created_at;
  return '';
}

/** `exactOptionalPropertyTypes` forbids writing `undefined` into an optional field, so absent fields are omitted rather than set. */
function optional<K extends string>(key: K, value: unknown): Record<string, unknown> {
  return value === undefined ? {} : { [key]: value };
}

function toChatMessage(row: MixedChatRow): ChatMessage {
  return {
    id: row.id ?? '',
    role: row.role ?? 'assistant',
    // `ChatMessage.name` is required and drives the displayed author label.
    // The index panel's rows carry none, and the baseline's own renderer
    // fell back to the participant type — `''` lets `ChatMessageList`'s own
    // participant-name resolution take over rather than printing a
    // made-up name.
    name: row.name ?? '',
    content: row.content ?? '',
    createdAt: resolveCreatedAt(row),
    ...optional('participantId', row.participantId ?? row.participant_id),
    ...optional('taskId', row.taskId ?? row.task_id),
    ...optional('toolActions', row.toolActions),
    ...optional('exception', row.exception),
    ...optional('isStreaming', row.isStreaming),
    ...optional('isLoading', row.isLoading),
  };
}

/**
 * The two injected-prop shapes, restated locally rather than re-exported
 * from `features/toolkits` — they are declared in
 * `features/toolkits/indexes/ui/IndexDetails/IndexChat.tsx`
 * (`ChatMessageListProps`/`LLMModelSelectorProps`), which `pages/` may not
 * deep-import (R-L3), and promoting two more type symbols onto that slice's
 * already-full §3.5 public API to spend them at this single call site is not
 * worth it. `INDEXES_CHAT_UI` below is typed as `IndexesTabChatUI`, so
 * TypeScript checks these declarations against the real contract
 * structurally — a drift in either one fails the build here.
 */
interface InjectedChatMessageListProps {
  readonly chat_history: readonly unknown[];
  readonly activeConversation: unknown;
  readonly isLoading: boolean;
  readonly isStreaming: boolean;
  readonly isLoadingMore: boolean;
  readonly interaction_uuid: string;
  readonly onCopyToClipboard: (id: string) => void;
}

interface InjectedLLMModelSelectorProps {
  readonly selectedModel: unknown;
  readonly onSelectModel: (model: unknown) => void;
  readonly models: readonly unknown[];
  readonly llmSettings: Record<string, unknown> | undefined;
  readonly onSetLLMSettings: (settings: Record<string, unknown>) => void;
}

function IndexChatMessageList(props: InjectedChatMessageListProps): ReactNode {
  const { chat_history, isStreaming, isLoadingMore, onCopyToClipboard } = props;

  const chatHistory = useMemo(() => chat_history.map((row) => toChatMessage(row as MixedChatRow)), [chat_history]);

  return (
    <RealChatMessageList
      chatHistory={chatHistory}
      isStreaming={isStreaming}
      // The slice's contract passes an id; this app's `ChatMessageList`
      // hands its callback the whole row. Bridged, not re-signatured.
      messageActions={{ onCopyToClipboard: (message: ChatMessage) => onCopyToClipboard(message.id) }}
      pagination={{ isLoadingMore }}
    />
  );
}

/** `widgets/llm-model-selector`'s own `LLMModel`/`LLMSettingsValues` are not on its barrel; the slice types both as `unknown` anyway, so the bridge is a cast at exactly one place. */
function IndexLLMModelSelector(props: InjectedLLMModelSelectorProps): ReactNode {
  const { selectedModel, onSelectModel, models, llmSettings, onSetLLMSettings } = props;

  type RealProps = ComponentProps<typeof RealLLMModelSelector>;
  return (
    <RealLLMModelSelector
      selectedModel={(selectedModel ?? null) as NonNullable<RealProps['selectedModel']> | null}
      onSelectModel={(model) => onSelectModel(model)}
      models={(models ?? []) as NonNullable<RealProps['models']>}
      llmSettings={llmSettings ?? {}}
      onSetLLMSettings={(settings) => onSetLLMSettings(settings as Record<string, unknown>)}
    />
  );
}

function IndexClearChatButton({ onClear }: { readonly onClear: () => void }): ReactNode {
  return <RealClearChatButton onClear={onClear} />;
}

/** Module-level constant: a fresh object here would remount the whole index detail panel on every `EditToolkit` render. */
export const INDEXES_CHAT_UI: IndexesTabChatUI = {
  LLMModelSelector: IndexLLMModelSelector,
  ChatMessageList: IndexChatMessageList,
  ClearChatButton: IndexClearChatButton,
};

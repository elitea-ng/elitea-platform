import type { Dispatch, ReactNode, Ref, SetStateAction } from 'react';
import type { PlusChatButtonEntitySubmenus } from '@/widgets/chat';
import type { ChatBoxEditorCallbacks } from './ChatBox.helpers';
import type { ChatBoxAgentEventSink, ChatBoxConversationProp } from './ChatBox.props';
import type { ConversationStarter } from './hooks/useChatBoxState';

/**
 * `ChatBox`'s imperative handle type — its own file purely to stay under the
 * §3.5 400-line file budget on both `ChatBox.tsx` and `ChatBox.helpers.ts`
 * (both already near the limit once `editorCallbacks`/`participant`
 * bundling landed).
 */

/**
 * @public Imperative handle proxied from `ChatBox` to whatever host mounts it
 * (baseline: `ChatPanel.jsx`'s `ref.stopAll`/`ref.onClear` — see
 * `features/pipelines`'s `ChatBoxSlotHandle`).
 *
 * `ChatBox` exposes this on its own `ref` PROP. It must never be attached to
 * the internal `chatInputRef`: that is the same ref `<NewChatInput
 * ref={chatInputRef}>` writes its `NewChatInputHandle` into, so attaching
 * here overwrites that handle on commit and every consumer of the input's
 * imperative surface (`getCursorPosition`/`replaceRange`/`reset`/`setValue`
 * — the "/" and "~" mention hooks, the voice button) throws "is not a
 * function" on each keystroke.
 */
export interface ChatBoxHandle {
  readonly onClear: () => void;
  readonly mentionUser: (content: string) => void;
  readonly stopAll: () => void;
}

/** @public Props for the ChatBox composition root. */
export interface ChatBoxProps {
  /** Host ref for the `ChatBoxHandle` (React 19 passes `ref` as a prop) — see `ChatBox.types.ts`. */
  readonly ref?: Ref<ChatBoxHandle> | undefined;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two), which the `ref` prop above pushed this component over — see `ChatBox.props.ts`. */
  readonly conversation?: ChatBoxConversationProp;
  readonly hidden?: boolean;
  readonly fromTheChat?: boolean;
  readonly projectId?: string | number;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of three). */
  readonly user?: { readonly id?: string; readonly name?: string; readonly avatar?: string };
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly participant?: { readonly active?: unknown; readonly onChange?: (participant: unknown) => void };
  readonly setChatHistory?: Dispatch<SetStateAction<readonly unknown[]>>;
  readonly conversationStarters?: readonly ConversationStarter[];
  readonly isAgentsPage?: boolean;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void };
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly onDelete?: { readonly answer?: (messageId: string) => void; readonly all?: () => void };
  /** Host-supplied composer extension points, bundled to stay under the §3.5 component-props budget (one slot instead of two, as `onDelete` above); both pass straight through. */
  readonly extensions?: {
    readonly contextIndicator?: ReactNode;
    /** Agent/pipeline editor open/close callbacks — see `ChatBox.helpers.ts`'s `buildAgentEditorProps`. Optional; falls back to the pre-existing no-ops. */
    readonly editorCallbacks?: ChatBoxEditorCallbacks;
    /** Real lists for the composer's "+" menu — see `processes/chat/model/usePlusMenuEntities.ts`, which is the only layer allowed to fetch them. */
    readonly entitySubmenus?: PlusChatButtonEntitySubmenus;
    readonly onAgentEvent?: ChatBoxAgentEventSink | undefined; // The run of the turn, not only its answer — see `ChatBoxAgentEventSink`.
  };
}

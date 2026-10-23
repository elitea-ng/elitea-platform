import { FRAME_TYPE } from '@elitea/client-core/stream';

/**
 * The chat stream's message types, as the provider emits them.
 *
 * A SUBSET of the generation stream's set, and deliberately its own constant
 * rather than an import: these are the types THIS reducer branches on, and a
 * shared enum would make the two screens' surfaces look identical when they are
 * not. `todo_update` is here and nowhere else; `start_task` is not here at all.
 * The values come from `@elitea/client-core`'s `FRAME_TYPE`, except
 * `todo_update`, which is not a chat stream frame.
 */
export const ChatFrameType = {
  Chunk: FRAME_TYPE.Chunk,
  AIMessageChunk: FRAME_TYPE.AIMessageChunk,
  AgentLlmChunk: FRAME_TYPE.AgentLlmChunk,
  AgentResponse: FRAME_TYPE.AgentResponse,
  AgentThinkingStep: FRAME_TYPE.AgentThinkingStep,
  AgentThinkingStepUpdate: FRAME_TYPE.AgentThinkingStepUpdate,
  AgentToolError: FRAME_TYPE.AgentToolError,
  AgentException: FRAME_TYPE.AgentException,
  Error: FRAME_TYPE.Error,
  LlmError: FRAME_TYPE.LlmError,
  /** Not an agent frame: deep research emits it as its own socket type. */
  TodoUpdate: 'todo_update',
} as const;

export type ChatFrameType = (typeof ChatFrameType)[keyof typeof ChatFrameType];

/** One frame off the chat stream. */
export interface ChatFrame {
  readonly type: string;
  readonly content?: unknown;
  readonly response_metadata?: Record<string, unknown> | undefined;
}

/**
 * Which of the two agents answered.
 *
 * It is carried on the ANSWER rather than read off the toggle, because the
 * toggle can move while a request is in flight and the label must describe what
 * actually ran. The legacy code kept it in `pendingCapabilityRef` for the same
 * reason.
 */
export type ChatCapability = 'ask' | 'research';

/** One entry in a research plan. Opaque: the widget renders whatever it holds. */
export interface ChatTodo {
  readonly id?: string | number;
  readonly title?: string;
  readonly status?: string;
}

/** One card inside an open thinking block. */
export interface ChatThinkingStep {
  readonly id: string;
  readonly event: string;
  readonly message: string;
  readonly data?: unknown;
  /** Only the plain-text path sets this: the frame type it arrived on. */
  readonly type?: string;
}

export interface ChatThinkingBlock {
  readonly type: 'thinking_steps';
  readonly id: string;
  readonly status: 'running' | 'completed';
  readonly steps: readonly ChatThinkingStep[];
}

export interface ChatTurn {
  readonly role: 'user' | 'assistant';
  readonly content: string;
  readonly sources?: readonly unknown[];
  readonly isError?: boolean;
  readonly capability?: ChatCapability;
}

export type ChatMessage = ChatTurn | ChatThinkingBlock;

export function isThinkingBlock(message: ChatMessage): message is ChatThinkingBlock {
  return (message as ChatThinkingBlock).type === 'thinking_steps';
}

/**
 * The whole reducer state.
 *
 * `streamId`, `activeBlockId` and `pendingCapability` were three refs in the
 * legacy component. They are state here because they are read BY the reduction,
 * not merely around it — a ref made them invisible to any test that did not
 * mount the component.
 *
 * `streamingText` is the answer AS IT ARRIVES. See the reducer's note: the
 * legacy component accumulated the same text into state nothing rendered.
 */
export interface ChatState {
  readonly messages: readonly ChatMessage[];
  readonly todos: readonly ChatTodo[] | null;
  readonly activeBlockId: string | null;
  readonly pendingCapability: ChatCapability | null;
  readonly streamId: string | null;
  readonly messageId: string | null;
  readonly mode: ChatCapability;
  readonly isLoading: boolean;
  readonly error: string | null;
  /**
   * The partial answer the provider has streamed so far.
   *
   * Cleared when a turn opens and when one COMPLETES — the finished answer
   * replaces it, and leaving it would show the same text twice. It is NOT
   * cleared by a failure: an interrupted stream leaving the partial text
   * visible is the whole point of keeping it (DWIKI-012).
   */
  readonly streamingText: string;
}

/**
 * A side effect the reducer asks its caller to perform.
 *
 * Returned rather than performed, which is what makes the reducer a pure
 * function and therefore replayable against a recording.
 */
type ChatEffect =
  | { readonly kind: 'persistCapability'; readonly capability: ChatCapability }
  | { readonly kind: 'unsubscribe' };

export interface ChatResult {
  readonly state: ChatState;
  readonly effects: readonly ChatEffect[];
}

export const initialChatState: ChatState = {
  messages: [],
  todos: null,
  activeBlockId: null,
  pendingCapability: null,
  streamId: null,
  messageId: null,
  mode: 'ask',
  isLoading: false,
  error: null,
  streamingText: '',
};

/**
 * The cap the legacy component put on one run's thinking log.
 *
 * It trims from the FRONT, so a long run keeps its most recent steps. Exported
 * because two frame families apply it and a second copy would be a second
 * number to change.
 */
export const MAX_THINKING_STEPS_PER_RUN = 200;

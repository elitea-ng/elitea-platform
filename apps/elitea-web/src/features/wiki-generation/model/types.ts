import { FRAME_TYPE } from '@elitea/client-core/stream';

/** The generation stream's message types, as the provider emits them: the subset of `@elitea/client-core`'s `FRAME_TYPE` it branches on. */
export const GenerationFrameType = {
  StartTask: FRAME_TYPE.StartTask,
  Chunk: FRAME_TYPE.Chunk,
  AIMessageChunk: FRAME_TYPE.AIMessageChunk,
  AgentResponse: FRAME_TYPE.AgentResponse,
  AgentToolStart: FRAME_TYPE.AgentToolStart,
  AgentToolEnd: FRAME_TYPE.AgentToolEnd,
  AgentToolError: FRAME_TYPE.AgentToolError,
  AgentLlmChunk: FRAME_TYPE.AgentLlmChunk,
  AgentThinkingStep: FRAME_TYPE.AgentThinkingStep,
  AgentThinkingStepUpdate: FRAME_TYPE.AgentThinkingStepUpdate,
  AgentException: FRAME_TYPE.AgentException,
  Error: FRAME_TYPE.Error,
  LlmError: FRAME_TYPE.LlmError,
} as const;

export type GenerationFrameType =
  (typeof GenerationFrameType)[keyof typeof GenerationFrameType];

/** One frame off the generation stream. */
export interface GenerationFrame {
  readonly type: string;
  readonly message_id?: string;
  readonly stream_id?: string;
  readonly content?: unknown;
  readonly response_metadata?: Record<string, unknown> | undefined;
}

/** What the user is told a generation is doing. */
// Not exported: nothing outside this module names it yet. It becomes public
// when a component renders the status directly (DWIKI-005).
interface GenerationStatus {
  readonly status: 'idle' | 'running' | 'completed' | 'error';
  readonly message: string;
}

/** One entry in the visible thinking log. */
export interface ThinkingStep {
  readonly id: string;
  readonly message: string;
  readonly timestamp: number;
  readonly type: string;
  readonly metadata?: Record<string, unknown> | undefined;
}

/**
 * The whole reducer state.
 *
 * `errored` is the flag the legacy code kept in a ref and consulted in one
 * branch out of five. Here it is state, and every branch that can finish a run
 * consults it — see the reducer's own note on the divergence.
 */
export interface GenerationState {
  readonly status: GenerationStatus;
  readonly thinkingSteps: readonly ThinkingStep[];
  readonly errored: boolean;
  readonly taskId: string | null;
  readonly stepCounter: number;
}

/**
 * A side effect the reducer asks its caller to perform.
 *
 * Returned rather than performed, which is what makes the reducer a pure
 * function and therefore replayable against a recording. The legacy version
 * called `cleanupGeneration` and `loadArtifactsList` directly from inside the
 * switch, which is why it could only be tested by mounting the component.
 */
// Not exported: it is reached through GenerationResult, which is.
type GenerationEffect =
  | { readonly kind: 'cleanup' }
  | { readonly kind: 'reloadArtifacts' }
  | { readonly kind: 'persistTaskId'; readonly taskId: string };

export interface GenerationResult {
  readonly state: GenerationState;
  readonly effects: readonly GenerationEffect[];
}

export const initialGenerationState: GenerationState = {
  status: { status: 'idle', message: '' },
  thinkingSteps: [],
  errored: false,
  taskId: null,
  stepCounter: 0,
};

/**
 * The model a desktop turn runs on (IPC.md "Model per turn", issue 1228):
 * the choice `agent_turn_start` carries, the `model` a turn's `status`
 * `running` event names, the reasoning events, and the workspace setting
 * that turns the choice off. Wire shapes are the host's (snake_case).
 *
 * The host checks a choice before the turn starts (`model_pinned`,
 * `model_override_disabled`, `model_unavailable`) and keeps it as the
 * thread's model; the gateway governs every request. Nothing here decides
 * what a person may use.
 */
import { tauriInvoke } from './hostBridge';

/**
 * The canonical reasoning setting (ADR-0038 decision 1): what
 * `llm_settings.reasoning_effort` stores and the host accepts.
 */
export type ReasoningEffort = 'auto' | 'off' | 'low' | 'medium' | 'high';

export const REASONING_EFFORTS: readonly ReasoningEffort[] = ['auto', 'off', 'low', 'medium', 'high'];

function isReasoningEffort(value: unknown): value is ReasoningEffort {
  return typeof value === 'string' && (REASONING_EFFORTS as readonly string[]).includes(value);
}

/**
 * A stored or reported effort as the canonical value: `none` (an agent saved
 * before ADR-0038, and the host's name for off in a `running` event) reads as
 * `off`. `undefined` for anything else, so an unknown value is never sent.
 */
export function toReasoningEffort(value: unknown): ReasoningEffort | undefined {
  if (value === 'none') return 'off';
  return isReasoningEffort(value) ? value : undefined;
}

/** `agent_turn_start`'s `model`: another chat model of the project for this turn (and the thread's next ones). */
export interface TurnModelChoice {
  name: string;
  reasoning_effort?: ReasoningEffort | undefined;
}

/** The `status` `running` event's `model`: what the turn runs on. */
export interface TurnModel {
  name: string;
  /** The host names off `none` here; read it with {@link toReasoningEffort}. */
  reasoning_effort?: ReasoningEffort | 'none';
  /** The person's choice, over the agent's own model. */
  chosen: boolean;
  /** The agent pins its model: no choice is offered. */
  pinned: boolean;
}

/** `thinking_delta`: the model's reasoning as it streams. */
export interface ThinkingDeltaPayload {
  text: string;
}

/** `thinking_end`: a reasoning block closed; `chars` is its length. */
export interface ThinkingEndPayload {
  chars: number;
}

/**
 * The workspace's `allow_model_override` (`workspace_agent_settings_get`):
 * `false` hides the composer's model chip. Outside the desktop host, or
 * when the host cannot answer, the chip shows (the host refuses a choice
 * the setting forbids anyway).
 */
export async function modelOverrideAllowed(workspaceId: string): Promise<boolean> {
  const invoke = tauriInvoke();
  if (invoke === undefined) return true;
  try {
    const settings = await invoke<{ allow_model_override?: boolean | null }>('workspace_agent_settings_get', { id: workspaceId });
    return settings.allow_model_override !== false;
  } catch {
    return true;
  }
}

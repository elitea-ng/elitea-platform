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

/** What `llm_settings.reasoning_effort` and the host accept. */
export type ReasoningEffort = 'none' | 'low' | 'medium' | 'high';

export const REASONING_EFFORTS: readonly ReasoningEffort[] = ['none', 'low', 'medium', 'high'];

export function isReasoningEffort(value: unknown): value is ReasoningEffort {
  return typeof value === 'string' && (REASONING_EFFORTS as readonly string[]).includes(value);
}

/** `agent_turn_start`'s `model`: another chat model of the project for this turn (and the thread's next ones). */
export interface TurnModelChoice {
  name: string;
  reasoning_effort?: ReasoningEffort | undefined;
}

/** The `status` `running` event's `model`: what the turn runs on. */
export interface TurnModel {
  name: string;
  reasoning_effort?: ReasoningEffort;
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

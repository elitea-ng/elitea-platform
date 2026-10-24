/**
 * A workspace's step limit (`workspace_agent_settings_*`, IPC.md): the model
 * steps of each run of its turns. A number, `'unlimited'`, or `null` for the
 * host's default ([`DEFAULT_STEP_LIMIT`]). At the limit the host asks
 * whether to continue.
 */
import { tauriInvoke } from './hostBridge';

export type StepLimit = number | 'unlimited';

/** The steps of a run when the workspace sets none (the host's `DEFAULT_STEP_LIMIT`). */
export const DEFAULT_STEP_LIMIT = 1024;

/** The most steps a number may set (the host's `STEP_LIMIT_RANGE`). */
export const MAX_STEP_LIMIT = 1_000_000;

type AgentSettings = Record<string, unknown> & { step_limit?: StepLimit | null };

function host(): NonNullable<ReturnType<typeof tauriInvoke>> {
  const invoke = tauriInvoke();
  if (invoke === undefined) throw new Error('The step limit is set in the desktop app.');
  return invoke;
}

/** The workspace's own step limit; `null`: the default. */
export async function readStepLimit(workspaceId: string): Promise<StepLimit | null> {
  const settings = await host()<AgentSettings>('workspace_agent_settings_get', { id: workspaceId });
  return settings.step_limit ?? null;
}

/** Store the workspace's step limit (`null`: back to the default), keeping its other settings; the next turn uses it. */
export async function writeStepLimit(workspaceId: string, limit: StepLimit | null): Promise<StepLimit | null> {
  const invoke = host();
  const settings = await invoke<AgentSettings>('workspace_agent_settings_get', { id: workspaceId });
  const saved = await invoke<AgentSettings>('workspace_agent_settings_set', { id: workspaceId, settings: { ...settings, step_limit: limit } });
  return saved.step_limit ?? null;
}

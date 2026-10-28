/**
 * The local thread's modes beside the composer: Act | Plan | Bypass
 * (`useBypassMode`, kept per folder; Plan wins). Bypass does not apply to
 * the cloud copy or a pipeline's cloud run (`local: false`).
 */
import type { TurnMode } from '@/shared/desktop/workspaceIpc';

import { useBypassMode, type BypassMode } from './useBypassMode';
import type { SessionCommands } from './useSessionCommands';

export interface ComposerModes {
  /** The mode the next turn starts in. */
  mode: TurnMode;
  bypass: BypassMode;
  /** The bypass notice shows (a local thread in bypass mode, not in plan mode). */
  bypassActive: boolean;
  /** The composer's mode switch: Bypass is offered for a local thread only. */
  switchProps: { bypass: boolean; onBypassChange: ((on: boolean) => void) | undefined };
}

export function useComposerModes(workspaceId: string, local: boolean, commands: SessionCommands): ComposerModes {
  const bypass = useBypassMode(workspaceId);
  const on = local && bypass.on;
  let mode: TurnMode = on ? 'bypass' : 'act';
  if (commands.planMode) mode = 'plan';
  return {
    mode,
    bypass,
    bypassActive: mode === 'bypass',
    switchProps: { bypass: on, onBypassChange: local ? (next) => bypass.request(next, () => commands.setPlanMode(false)) : undefined },
  };
}

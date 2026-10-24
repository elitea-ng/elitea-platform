/**
 * The local thread's modes beside the composer: Act | Plan | Bypass
 * (`useBypassMode`, kept per folder; Plan wins) and the idle compaction that
 * arms the next message (`useIdleCompaction`). Neither applies to the cloud
 * copy or a pipeline's cloud run (`local: false`).
 */
import type { WorkspaceTurn } from '@/features/workspace';
import type { TurnMode } from '@/shared/desktop/workspaceIpc';

import { threadMeter } from './contextBreakdown';
import { useBypassMode, type BypassMode } from './useBypassMode';
import { useIdleCompaction } from './useIdleCompaction';
import type { SessionCommands } from './useSessionCommands';
import type { ThreadHistory } from './useThreadHistory';

export interface ComposerModes {
  /** The mode the next turn starts in. */
  mode: TurnMode;
  bypass: BypassMode;
  /** The bypass notice shows (a local thread in bypass mode, not in plan mode). */
  bypassActive: boolean;
  /** The composer's mode switch: Bypass is offered for a local thread only. */
  switchProps: { bypass: boolean; onBypassChange: ((on: boolean) => void) | undefined };
}

export function useComposerModes(workspaceId: string, local: boolean, turn: WorkspaceTurn, history: ThreadHistory, commands: SessionCommands): ComposerModes {
  const bypass = useBypassMode(workspaceId);
  useIdleCompaction({ meter: local ? threadMeter(turn, history) : undefined, busy: turn.busy, armed: commands.compactNext, arm: commands.armIdleCompact, local });
  const on = local && bypass.on;
  let mode: TurnMode = on ? 'bypass' : 'act';
  if (commands.planMode) mode = 'plan';
  return {
    mode,
    bypass,
    bypassActive: mode === 'bypass',
    switchProps: { bypass: on, onBypassChange: local ? bypass.request : undefined },
  };
}

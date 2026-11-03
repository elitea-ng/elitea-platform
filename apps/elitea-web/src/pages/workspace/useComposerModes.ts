/**
 * The local thread's modes beside the composer: Act | Plan | Bypass |
 * Unattended (`useBypassMode`, `useUnattendedMode`, each kept per folder;
 * Plan wins, and Bypass and Unattended turn each other off). Neither applies
 * to the cloud copy or a pipeline's cloud run (`local: false`).
 */
import { toWireGrant } from '@/features/unattended';
import type { TurnMode, TurnModeChoice } from '@/shared/desktop/workspaceIpc';

import { useBypassMode, type BypassMode } from './useBypassMode';
import type { SessionCommands } from './useSessionCommands';
import { useUnattendedMode, type UnattendedMode } from './useUnattendedMode';

export interface ComposerModes {
  /** The mode the next turn starts in. */
  mode: TurnMode;
  /** What a send passes: the mode, or Unattended with its grant. */
  choice: TurnModeChoice;
  bypass: BypassMode;
  unattended: UnattendedMode;
  /** The bypass notice shows (a local thread in bypass mode, not in plan mode). */
  bypassActive: boolean;
  /** The unattended notice shows (a local thread in unattended mode, not in plan mode). */
  unattendedActive: boolean;
  /** The composer's mode switch: Bypass and Unattended are offered for a local thread only. */
  switchProps: {
    bypass: boolean;
    onBypassChange: ((on: boolean) => void) | undefined;
    unattended: boolean;
    onUnattendedChange: ((on: boolean) => void) | undefined;
  };
}

/** The mode of the next turn, and the checked grant when it is Unattended. */
function modeOf(planMode: boolean, unattended: TurnModeChoice | null, bypass: boolean): { mode: TurnMode; choice: TurnModeChoice } {
  if (planMode) return { mode: 'plan', choice: 'plan' };
  if (unattended !== null) return { mode: 'unattended', choice: unattended };
  return bypass ? { mode: 'bypass', choice: 'bypass' } : { mode: 'act', choice: 'act' };
}

export function useComposerModes(workspaceId: string, local: boolean, commands: SessionCommands): ComposerModes {
  const bypass = useBypassMode(workspaceId);
  const unattended = useUnattendedMode(workspaceId);
  const wire = local && unattended.grant !== null ? toWireGrant(unattended.grant) : null;
  const granted: TurnModeChoice | null = wire?.ok === true ? { mode: 'unattended', grant: wire.grant } : null;
  const { mode, choice } = modeOf(commands.planMode, granted, local && bypass.on);
  const onBypassChange = (next: boolean): void =>
    bypass.request(next, () => {
      commands.setPlanMode(false);
      unattended.request(false);
    });
  const onUnattendedChange = (next: boolean): void =>
    unattended.request(next, () => {
      commands.setPlanMode(false);
      bypass.request(false);
    });
  return {
    mode,
    choice,
    bypass,
    unattended,
    bypassActive: mode === 'bypass',
    unattendedActive: mode === 'unattended',
    switchProps: {
      bypass: local && bypass.on && granted === null,
      onBypassChange: local ? onBypassChange : undefined,
      unattended: granted !== null,
      onUnattendedChange: local ? onUnattendedChange : undefined,
    },
  };
}

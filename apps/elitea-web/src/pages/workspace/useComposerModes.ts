/**
 * The local thread's modes beside the composer: Act | Plan | Bypass
 * (`useBypassMode`, kept per folder; Plan wins), and "Run to completion"
 * (`useRunToCompletion`, kept per folder, on by default), which works with
 * each mode. In Plan mode no grant goes out: a plan proposal waits for the
 * person. Neither applies to the cloud copy or a pipeline's cloud run
 * (`local: false`). When an administrator turned grants off, the control
 * hides and no grant goes out.
 */
import { grantToSend, toWireGrant, useGrantsTurnedOff, type RunChoice } from '@/features/unattended';
import type { TurnMode, TurnModeChoice } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { useBypassMode, type BypassMode } from './useBypassMode';
import { useRunToCompletion } from './useRunToCompletion';
import type { SessionCommands } from './useSessionCommands';

export interface RunToCompletionBinding {
  value: RunChoice;
  onChange: (next: RunChoice) => void;
  /** Why the choice does not apply to the next send (Plan mode). */
  notApplied: string | undefined;
}

export interface ComposerModes {
  /** The mode the next turn starts in. */
  mode: TurnMode;
  /** What a send passes: the mode, with the grant while "Run to completion" is on. */
  choice: TurnModeChoice;
  bypass: BypassMode;
  /** The bypass notice shows (a local thread in bypass mode, not in plan mode). */
  bypassActive: boolean;
  /** The composer's mode switch: Bypass is offered for a local thread only. */
  switchProps: {
    bypass: boolean;
    onBypassChange: ((on: boolean) => void) | undefined;
  };
  /** The "Run to completion" control; undefined when it does not show. */
  runToCompletion: RunToCompletionBinding | undefined;
}

function modeOf(planMode: boolean, bypass: boolean): TurnMode {
  if (planMode) return 'plan';
  return bypass ? 'bypass' : 'act';
}

/** The send's choice: the mode, with the checked grant when the control is on (never in Plan mode). */
function choiceOf(mode: TurnMode, run: RunChoice | null): TurnModeChoice {
  if (mode === 'plan' || run === null) return mode;
  const grant = grantToSend(run, 'desktop');
  const wire = grant === null ? null : toWireGrant(grant);
  return wire?.ok === true ? { mode, grant: wire.grant } : mode;
}

export function useComposerModes(workspaceId: string, local: boolean, commands: SessionCommands): ComposerModes {
  const bypass = useBypassMode(workspaceId);
  const run = useRunToCompletion(workspaceId);
  const grantsOff = useGrantsTurnedOff();
  const mode = modeOf(commands.planMode, local && bypass.on);
  const offered = local && !grantsOff;
  const onBypassChange = (next: boolean): void => bypass.request(next, () => commands.setPlanMode(false));
  return {
    mode,
    choice: choiceOf(mode, offered ? run.choice : null),
    bypass,
    bypassActive: mode === 'bypass',
    switchProps: {
      bypass: local && bypass.on,
      onBypassChange: local ? onBypassChange : undefined,
    },
    runToCompletion: offered
      ? {
          value: run.choice,
          onChange: run.setChoice,
          notApplied: mode === 'plan' ? t('workspace.runToCompletion.planMode', 'Run to completion is not used in Plan mode: the plan waits for you. It applies in Act and Bypass mode.') : undefined,
        }
      : undefined,
  };
}

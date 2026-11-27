/**
 * The local thread's modes beside the composer: Act | Plan | Bypass | Full
 * auto (`useBypassMode`, kept per folder; `useFullAutoMode`, this thread
 * only and never kept; Plan wins, then Full auto), and "Run to completion"
 * (`useRunToCompletion`, kept per folder, on by default), which works with
 * each mode. In Plan mode no grant goes out: a plan proposal waits for the
 * person. Neither applies to the cloud copy or a pipeline's cloud run
 * (`local: false`). When an administrator turned grants off, the control
 * hides and no grant goes out; when an administrator forbids Full auto, it
 * is not offered.
 */
import { grantToSend, toWireGrant, useGrantsTurnedOff, type RunChoice } from '@/features/unattended';
import type { TurnMode, TurnModeChoice } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { FullAutoSwitch } from './ModePicker';
import type { PreThreadChoices } from './preThreadChoices';
import { useBypassMode, type BypassMode } from './useBypassMode';
import { useFullAutoMode, type FullAutoMode } from './useFullAutoMode';
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
  fullAuto: FullAutoMode;
  /** The composer's mode switch: Bypass and Full auto are offered for a local thread only. */
  switchProps: {
    bypass: boolean;
    onBypassChange: ((on: boolean) => void) | undefined;
    fullAuto: FullAutoSwitch | undefined;
  };
  /** The "Run to completion" control; undefined when it does not show. */
  runToCompletion: RunToCompletionBinding | undefined;
  /** The modes as chosen now, for the thread a first message creates in a new worktree (#3144). */
  choices: () => PreThreadChoices;
  /** A handed mode is still being checked (Full auto's policy): the first message waits. */
  settling: boolean;
}

function modeOf(planMode: boolean, fullAuto: boolean, bypass: boolean): TurnMode {
  if (planMode) return 'plan';
  if (fullAuto) return 'full_auto';
  return bypass ? 'bypass' : 'act';
}

/** The send's choice: the mode, with the checked grant when the control is on (never in Plan mode). */
function choiceOf(mode: TurnMode, run: RunChoice | null): TurnModeChoice {
  if (mode === 'plan' || run === null) return mode;
  const grant = grantToSend(run, 'desktop');
  const wire = grant === null ? null : toWireGrant(grant);
  return wire?.ok === true ? { mode, grant: wire.grant } : mode;
}

const NOT_HANDED: Partial<PreThreadChoices> = {};

/** "Run to completion" as chosen, when the control is offered (grants on). */
const offeredRun = (offered: boolean, choice: RunChoice): RunChoice | undefined => (offered ? choice : undefined);

/** `handed`: the modes a worktree's new thread was handed with its first message (#3144); it starts in them. */
export function useComposerModes(workspaceId: string, local: boolean, commands: SessionCommands, handed: Partial<PreThreadChoices> = NOT_HANDED): ComposerModes {
  const bypass = useBypassMode(workspaceId, handed.bypass);
  const rawFullAuto = useFullAutoMode(local, commands.planMode, handed.fullAuto);
  const run = useRunToCompletion(workspaceId, handed.runToCompletion);
  const grantsOff = useGrantsTurnedOff();
  const mode = modeOf(commands.planMode, local && rawFullAuto.on, local && bypass.on);
  const offered = local && !grantsOff;
  // Picking Bypass leaves Full auto once Bypass is on (a cancelled warning keeps the mode); picking Act or Plan leaves both.
  const onBypassChange = (next: boolean): void => {
    if (!next) rawFullAuto.request(false);
    bypass.request(next, () => {
      rawFullAuto.request(false);
      commands.setPlanMode(false);
    });
  };
  // Turning Full auto off (the banner) returns to Act, not to a Bypass kept for the folder.
  const fullAuto: FullAutoMode = {
    ...rawFullAuto,
    request: (next, turnedOn) => {
      if (!next && rawFullAuto.on) bypass.request(false);
      rawFullAuto.request(next, turnedOn);
    },
  };
  // Full auto turns plan mode off only once the person confirmed it.
  const onFullAutoChange = (next: boolean): void => fullAuto.request(next, () => commands.setPlanMode(false));
  return {
    mode,
    choice: choiceOf(mode, offered ? run.choice : null),
    bypass,
    bypassActive: mode === 'bypass',
    choices: () => ({ planMode: commands.planMode, bypass: bypass.on, fullAuto: rawFullAuto.on, runToCompletion: offeredRun(offered, run.choice) }),
    settling: rawFullAuto.settling,
    fullAuto,
    switchProps: {
      bypass: local && bypass.on,
      onBypassChange: local ? onBypassChange : undefined,
      fullAuto: local && rawFullAuto.allowed ? { on: rawFullAuto.on, onChange: onFullAutoChange } : undefined,
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

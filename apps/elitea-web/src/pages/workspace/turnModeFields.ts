/**
 * The mode fields of `agent_turn_start`: `plan_mode`, `bypass_approvals` only
 * when the Bypass mode is on, `bypass_approvals` and `full_auto` in Full auto
 * (issue 3079), and `unattended` (the "Run to completion" grant, with a new
 * grant id: each message the person sends starts a new chain) when the choice
 * carries one. The grant never sets `bypass_approvals` or `full_auto`, and it
 * never goes with `plan_mode` (the host refuses that pair): a plan proposal
 * waits for the person.
 */
import { newGrantId } from '@/features/unattended';
import type { TurnMode, TurnModeChoice, TurnStartRequest } from '@/shared/desktop/workspaceIpc';

type ModeFields = Pick<TurnStartRequest, 'plan_mode' | 'bypass_approvals' | 'full_auto' | 'unattended'>;

function fieldsOf(mode: TurnMode): ModeFields {
  switch (mode) {
    case 'plan':
      return { plan_mode: true };
    case 'bypass':
      return { plan_mode: false, bypass_approvals: true };
    case 'full_auto':
      return { plan_mode: false, bypass_approvals: true, full_auto: true };
    case 'act':
      return { plan_mode: false };
  }
}

export function modeFields(choice: TurnModeChoice): ModeFields {
  const mode = typeof choice === 'object' ? choice.mode : choice;
  const fields = fieldsOf(mode);
  if (mode === 'plan') return fields;
  return typeof choice === 'object' ? { ...fields, unattended: { ...choice.grant, grant_id: newGrantId() } } : fields;
}

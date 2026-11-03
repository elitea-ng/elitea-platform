/**
 * The mode fields of `agent_turn_start`: `plan_mode`, `bypass_approvals` only
 * when the Bypass mode is on, and `unattended` (the "Run to completion" grant,
 * with a new grant id: each message the person sends starts a new chain) when
 * the choice carries one. The grant never sets `bypass_approvals`, and it
 * never goes with `plan_mode` (the host refuses that pair): a plan proposal
 * waits for the person.
 */
import { newGrantId } from '@/features/unattended';
import type { TurnModeChoice, TurnStartRequest } from '@/shared/desktop/workspaceIpc';

export function modeFields(choice: TurnModeChoice): Pick<TurnStartRequest, 'plan_mode' | 'bypass_approvals' | 'unattended'> {
  const mode = typeof choice === 'object' ? choice.mode : choice;
  if (mode === 'plan') return { plan_mode: true };
  const fields: Pick<TurnStartRequest, 'plan_mode' | 'bypass_approvals' | 'unattended'> = mode === 'bypass' ? { plan_mode: false, bypass_approvals: true } : { plan_mode: false };
  return typeof choice === 'object' ? { ...fields, unattended: { ...choice.grant, grant_id: newGrantId() } } : fields;
}

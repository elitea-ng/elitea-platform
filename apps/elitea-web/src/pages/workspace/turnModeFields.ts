/**
 * The mode fields of `agent_turn_start`: `plan_mode`, `bypass_approvals` only
 * when Bypass is on, and `unattended` (the grant, with a new grant id: each
 * message the person sends starts a new chain) when Unattended is on.
 * Unattended implies bypass on the host, so it does not send
 * `bypass_approvals`.
 */
import { newGrantId } from '@/features/unattended';
import type { TurnModeChoice, TurnStartRequest } from '@/shared/desktop/workspaceIpc';

export function modeFields(mode: TurnModeChoice): Pick<TurnStartRequest, 'plan_mode' | 'bypass_approvals' | 'unattended'> {
  if (typeof mode === 'object') return { plan_mode: false, unattended: { ...mode.grant, grant_id: newGrantId() } };
  return mode === 'bypass' ? { plan_mode: false, bypass_approvals: true } : { plan_mode: mode === 'plan' };
}

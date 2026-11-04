/**
 * "Run to completion" (ADR-0045, owner decision 2026-11): the composer's one
 * control for an unattended grant. It is on by default: each new request
 * continues by itself at the step limit, at the desktop cap of 512 tool calls
 * and at the cloud output limit, inside the chain limits. The parts that
 * answer for the person (hook asks, remote confirmations, sensitive tools)
 * stay off until the person turns each one on.
 *
 * The choice does not make a turn bypass the approval rules: on the desktop,
 * only the Bypass mode does that.
 */
import { isRecord } from '@/shared/lib/object';

import { defaultGrant, readGrant, type UnattendedGrantValue } from './grant';

/** The control's state: `on` sends the grant; `grant` keeps the person's settings while it is off too. */
export interface RunChoice {
  on: boolean;
  grant: UnattendedGrantValue;
}

/** Where the control shows: the desktop shows hook asks and remote confirmations, the cloud the sensitive tools. */
export type RunScope = 'desktop' | 'cloud';

/** A new conversation or folder: on, with the default grant. */
export function defaultChoice(): RunChoice {
  return { on: true, grant: defaultGrant() };
}

/** True when the grant also answers for the person (a part that is off by default is on). */
export function answersForPerson(grant: UnattendedGrantValue, scope: RunScope): boolean {
  if (scope === 'desktop') return grant.hook_asks || grant.remote_confirmations;
  return grant.sensitive_tools.length > 0;
}

/**
 * The grant that a send carries: the continue parts always on (the control
 * turns them on and off together), and only the parts of `scope`. Null while
 * the control is off.
 */
export function grantToSend(choice: RunChoice, scope: RunScope): UnattendedGrantValue | null {
  if (!choice.on) return null;
  const desktop = scope === 'desktop';
  return {
    ...choice.grant,
    continue_step_limit: true,
    continue_tool_call_cap: desktop,
    continue_output_limit: !desktop,
    hook_asks: desktop && choice.grant.hook_asks,
    remote_confirmations: desktop && choice.grant.remote_confirmations,
    sensitive_tools: desktop ? [] : choice.grant.sensitive_tools,
  };
}

/** A kept choice read back (stored JSON), or null when it is not valid. */
export function readChoice(raw: unknown): RunChoice | null {
  if (!isRecord(raw) || typeof raw['on'] !== 'boolean') return null;
  const grant = readGrant(raw['grant']);
  return grant === null ? null : { on: raw['on'], grant };
}

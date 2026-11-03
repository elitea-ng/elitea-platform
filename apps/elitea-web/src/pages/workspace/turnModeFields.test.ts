import { describe, expect, it } from 'vitest';

import { defaultGrant, toWireGrant } from '@/features/unattended';

import { modeFields } from './turnModeFields';

function grant() {
  const wire = toWireGrant({ ...defaultGrant(), chain: { max_continuations: '3', max_spend: '', deadline_hours: '' } });
  if (!wire.ok) throw new Error('the grant is valid');
  return wire.grant;
}

describe('modeFields', () => {
  it('keeps the act, plan and bypass wires as they were', () => {
    expect(modeFields('act')).toEqual({ plan_mode: false });
    expect(modeFields('plan')).toEqual({ plan_mode: true });
    expect(modeFields('bypass')).toEqual({ plan_mode: false, bypass_approvals: true });
  });

  it('sends the grant with a new grant id per send, without bypass_approvals', () => {
    const first = modeFields({ mode: 'act', grant: grant() });
    const second = modeFields({ mode: 'act', grant: grant() });
    expect(first).toEqual({
      plan_mode: false,
      unattended: {
        grant_id: expect.stringMatching(/^[0-9a-f-]{36}$/u) as string,
        continue_step_limit: true,
        continue_tool_call_cap: true,
        hook_asks: false,
        remote_confirmations: false,
        goal_check: true,
        chain: { max_continuations: 3 },
      },
    });
    expect(first).not.toHaveProperty('bypass_approvals');
    expect(second.unattended?.grant_id).not.toBe(first.unattended?.grant_id);
  });

  it('sends bypass_approvals only from the Bypass mode, and no grant in Plan mode', () => {
    expect(modeFields({ mode: 'bypass', grant: grant() })).toMatchObject({ plan_mode: false, bypass_approvals: true, unattended: { continue_step_limit: true } });
    expect(modeFields({ mode: 'plan', grant: grant() })).toEqual({ plan_mode: true });
  });
});

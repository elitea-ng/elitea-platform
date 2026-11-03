import { describe, expect, it } from 'vitest';

import { defaultGrant, toWireGrant } from '@/features/unattended';

import { modeFields } from './turnModeFields';

describe('modeFields', () => {
  it('keeps the act, plan and bypass wires as they were', () => {
    expect(modeFields('act')).toEqual({ plan_mode: false });
    expect(modeFields('plan')).toEqual({ plan_mode: true });
    expect(modeFields('bypass')).toEqual({ plan_mode: false, bypass_approvals: true });
  });

  it('sends the grant with a new grant id per send, without bypass_approvals', () => {
    const wire = toWireGrant({ ...defaultGrant(), chain: { max_continuations: '3', max_spend: '', deadline_hours: '' } });
    if (!wire.ok) throw new Error('the grant is valid');
    const first = modeFields({ mode: 'unattended', grant: wire.grant });
    const second = modeFields({ mode: 'unattended', grant: wire.grant });
    expect(first).toEqual({
      plan_mode: false,
      unattended: {
        grant_id: expect.stringMatching(/^[0-9a-f-]{36}$/u) as string,
        continue_step_limit: true,
        continue_tool_call_cap: true,
        hook_asks: false,
        remote_confirmations: false,
        chain: { max_continuations: 3 },
      },
    });
    expect(first).not.toHaveProperty('bypass_approvals');
    expect(second.unattended?.grant_id).not.toBe(first.unattended?.grant_id);
  });
});

import { describe, expect, it } from 'vitest';

import { chainErrors, defaultGrant, newGrantId, readGrant, toWireGrant, type UnattendedGrantValue } from './grant';

const withChain = (chain: Partial<UnattendedGrantValue['chain']>): UnattendedGrantValue => ({ ...defaultGrant(), chain: { ...defaultGrant().chain, ...chain } });

describe('defaultGrant', () => {
  it('continues by itself and leaves the project guards to their defaults', () => {
    expect(defaultGrant()).toEqual({
      continue_step_limit: true,
      continue_tool_call_cap: true,
      continue_output_limit: true,
      hook_asks: false,
      remote_confirmations: false,
      sensitive_tools: [],
      chain: { max_continuations: '', max_spend: '', deadline_hours: '' },
    });
  });
});

describe('toWireGrant', () => {
  it('sends the four desktop switches and no chain when every limit is empty', () => {
    expect(toWireGrant(defaultGrant())).toEqual({
      ok: true,
      grant: { continue_step_limit: true, continue_tool_call_cap: true, hook_asks: false, remote_confirmations: false },
    });
  });

  it('never sends the cloud-only parts on the desktop wire', () => {
    const value = { ...defaultGrant(), continue_output_limit: true, sensitive_tools: [{ toolkit: 'github', tool: 'delete_repo' }] };
    const wire = toWireGrant(value);
    expect(wire.ok && Object.keys(wire.grant)).toEqual(['continue_step_limit', 'continue_tool_call_cap', 'hook_asks', 'remote_confirmations']);
  });

  it('adds the grant id when given, and only the typed chain limits', () => {
    expect(toWireGrant(withChain({ max_continuations: ' 5 ', deadline_hours: '1.5' }), 'g-1')).toEqual({
      ok: true,
      grant: { grant_id: 'g-1', continue_step_limit: true, continue_tool_call_cap: true, hook_asks: false, remote_confirmations: false, chain: { max_continuations: 5, deadline_seconds: 5400 } },
    });
    const spend = toWireGrant(withChain({ max_spend: '12.3456' }));
    expect(spend.ok && spend.grant.chain).toEqual({ max_spend: 12.3456 });
  });

  it('refuses limits out of range, and names the fields', () => {
    expect(toWireGrant(withChain({ max_continuations: '201', max_spend: '0', deadline_hours: '169' }))).toEqual({ ok: false, invalid: ['max_continuations', 'max_spend', 'deadline_hours'] });
  });
});

describe('chainErrors', () => {
  it.each([
    ['max_continuations', '0', true],
    ['max_continuations', '200', true],
    ['max_continuations', '200.0', false],
    ['max_continuations', '-1', false],
    ['max_continuations', 'ten', false],
    ['max_spend', '0.0001', true],
    ['max_spend', '.5', true],
    ['max_spend', '0.00001', false],
    ['max_spend', '0.0000', false],
    ['max_spend', '1e3', false],
    ['deadline_hours', '168', true],
    ['deadline_hours', '0.0003', true],
    ['deadline_hours', '0.0001', false],
    ['deadline_hours', '168.01', false],
  ] as const)('%s = %j is valid: %s', (field, text, valid) => {
    expect(chainErrors(withChain({ [field]: text }).chain).includes(field)).toBe(!valid);
  });
});

describe('readGrant', () => {
  it('reads a kept grant back, and drops what is not one', () => {
    const kept = withChain({ max_continuations: '3' });
    expect(readGrant(JSON.parse(JSON.stringify(kept)))).toEqual(kept);
    expect(readGrant({ ...kept, hook_asks: 'yes' })).toBeNull();
    expect(readGrant({ ...kept, chain: { max_continuations: '999', max_spend: '', deadline_hours: '' } })).toBeNull();
    expect(readGrant(null)).toBeNull();
    expect(readGrant({ ...kept, sensitive_tools: [{ toolkit: 'a', tool: 'b' }, { toolkit: 1 }] })?.sensitive_tools).toEqual([{ toolkit: 'a', tool: 'b' }]);
  });
});

describe('newGrantId', () => {
  it('makes a new uuid each time', () => {
    const [a, b] = [newGrantId(), newGrantId()];
    expect(a).toMatch(/^[0-9a-f-]{36}$/u);
    expect(a).not.toBe(b);
  });
});

import { describe, expect, it } from 'vitest';

import { defaultGrant } from './grant';
import { answersForPerson, defaultChoice, grantToSend, readChoice } from './runChoice';

describe('defaultChoice', () => {
  it('is on, continues by itself, and answers nothing for the person', () => {
    const choice = defaultChoice();
    expect(choice.on).toBe(true);
    expect(choice.grant).toEqual(defaultGrant());
    expect(answersForPerson(choice.grant, 'desktop')).toBe(false);
    expect(answersForPerson(choice.grant, 'cloud')).toBe(false);
  });
});

describe('grantToSend', () => {
  it('sends nothing while the choice is off', () => {
    expect(grantToSend({ ...defaultChoice(), on: false }, 'desktop')).toBeNull();
  });

  it('keeps only the parts of the scope', () => {
    const grant = { ...defaultGrant(), hook_asks: true, sensitive_tools: [{ toolkit: 'github', tool: 'merge' }] };
    expect(grantToSend({ on: true, grant }, 'desktop')).toMatchObject({ continue_step_limit: true, continue_tool_call_cap: true, continue_output_limit: false, hook_asks: true, sensitive_tools: [] });
    expect(grantToSend({ on: true, grant }, 'cloud')).toMatchObject({ continue_step_limit: true, continue_tool_call_cap: false, continue_output_limit: true, hook_asks: false, remote_confirmations: false, sensitive_tools: [{ toolkit: 'github', tool: 'merge' }] });
  });
});

describe('answersForPerson', () => {
  it('is true when a part that is off by default is on', () => {
    expect(answersForPerson({ ...defaultGrant(), remote_confirmations: true }, 'desktop')).toBe(true);
    expect(answersForPerson({ ...defaultGrant(), remote_confirmations: true }, 'cloud')).toBe(false);
    expect(answersForPerson({ ...defaultGrant(), sensitive_tools: [{ toolkit: 'a', tool: 'b' }] }, 'cloud')).toBe(true);
  });
});

describe('readChoice', () => {
  it('reads a kept choice back, and refuses a shape that is not one', () => {
    const kept = { on: false, grant: { ...defaultGrant(), hook_asks: true } };
    expect(readChoice(JSON.parse(JSON.stringify(kept)))).toEqual(kept);
    expect(readChoice({ on: 'yes', grant: defaultGrant() })).toBeNull();
    expect(readChoice({ on: true, grant: { continue_step_limit: true } })).toBeNull();
    expect(readChoice(null)).toBeNull();
  });
});

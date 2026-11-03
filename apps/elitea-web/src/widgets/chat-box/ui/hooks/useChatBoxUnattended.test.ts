import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { defaultChoice, defaultGrant, markGrantsTurnedOff, resetGrantsTurnedOff } from '@/features/unattended';

import { grantForSend, useChatBoxUnattended } from './useChatBoxUnattended';

const tools = [
  { toolkit: 'github', tool: 'delete_branch' },
  { toolkit: 'jira', tool: 'delete_issue' },
];

afterEach(resetGrantsTurnedOff);

describe('useChatBoxUnattended', () => {
  it('sends the default grant on each send, with a new grant id and no sensitive tools', () => {
    const { result } = renderHook(() => useChatBoxUnattended('conv-default', tools));
    expect(result.current.shown).toBe(true);
    const first = result.current.wire();
    expect(first).toEqual({ grant_id: expect.stringMatching(/^[0-9a-f-]{36}$/u) as string, continue_step_limit: true, continue_output_limit: true });
    expect(result.current.wire()?.['grant_id']).not.toBe(first?.['grant_id']);
  });

  it('sends nothing while the person turned it off, and keeps that choice for the conversation', () => {
    const { result, rerender } = renderHook(({ key }) => useChatBoxUnattended(key, tools), { initialProps: { key: 'conv-off' } });
    act(() => result.current.setChoice({ ...defaultChoice(), on: false }));
    expect(result.current.wire()).toBeUndefined();
    rerender({ key: 'conv-new' });
    expect(result.current.choice.on).toBe(true);
    rerender({ key: 'conv-off' });
    expect(result.current.choice.on).toBe(false);
  });

  it('hides and sends nothing when grants are off, or in the Test panel', () => {
    const { result } = renderHook(() => useChatBoxUnattended('conv-hidden', tools));
    act(markGrantsTurnedOff);
    expect(result.current.shown).toBe(false);
    expect(result.current.wire()).toBeUndefined();
    const panel = renderHook(() => useChatBoxUnattended('conv-panel', tools, false));
    expect(panel.result.current.shown).toBe(false);
  });
});

describe('grantForSend', () => {
  it('drops a named tool that the agent no longer has, and all of them without an agent', () => {
    const choice = { on: true, grant: { ...defaultGrant(), sensitive_tools: [{ toolkit: 'github', tool: 'delete_branch' }, { toolkit: 'old', tool: 'gone' }] } };
    expect(grantForSend(choice, tools, 'g-1')).toEqual({ grant_id: 'g-1', continue_step_limit: true, continue_output_limit: true, sensitive_tools: [{ toolkit_name: 'github', tool_name: 'delete_branch' }] });
    expect(grantForSend(choice, undefined, 'g-2')).toEqual({ grant_id: 'g-2', continue_step_limit: true, continue_output_limit: true });
    expect(grantForSend({ ...choice, on: false }, tools, 'g-3')).toBeUndefined();
  });
});

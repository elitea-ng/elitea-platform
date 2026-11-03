import { describe, expect, it } from 'vitest';

import { grantableTools, toCloudGrant, unattendedRefusalText } from './cloud';
import { defaultGrant } from './grant';

describe('toCloudGrant', () => {
  it('sends the cloud parts, the named tools and the typed chain limits, never a desktop part', () => {
    const result = toCloudGrant(
      { ...defaultGrant(), continue_output_limit: false, hook_asks: true, sensitive_tools: [{ toolkit: 'github', tool: 'delete_branch' }], chain: { max_continuations: '5', max_spend: '2.5', deadline_hours: '1.5' } },
      'g-1',
    );
    expect(result).toEqual({
      ok: true,
      grant: {
        grant_id: 'g-1',
        continue_step_limit: true,
        continue_output_limit: false,
        goal_check: true,
        sensitive_tools: [{ toolkit_name: 'github', tool_name: 'delete_branch' }],
        chain: { max_continuations: 5, max_spend: 2.5, deadline_seconds: 5400 },
      },
    });
  });

  it('leaves out the empty parts: Main takes the platform defaults', () => {
    expect(toCloudGrant(defaultGrant())).toEqual({ ok: true, grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true } });
  });

  it('sends goal_check off when the person turned the check off', () => {
    const result = toCloudGrant({ ...defaultGrant(), goal_check: false });
    expect(result.ok && result.grant.goal_check).toBe(false);
  });

  it('names the chain fields that are not valid', () => {
    expect(toCloudGrant({ ...defaultGrant(), chain: { max_continuations: '-5', max_spend: '', deadline_hours: 'x' } })).toEqual({ ok: false, invalid: ['max_continuations', 'deadline_hours'] });
  });
});

describe('grantableTools', () => {
  it('lists the selected tools of each toolkit once, and skips sub-agents and toolkits that select all tools', () => {
    const versionDetails = {
      tools: [
        { id: 1, name: 'github', type: 'github', selected_tools: ['delete_branch', 'create_pr', 'delete_branch'] },
        { id: 2, name: 'jira', type: 'jira', selected_tools: {}, config: { settings: { selected_tools: ['delete_issue'] } } },
        { id: 3, name: 'reviewer', type: 'application', settings: { selected_tools: ['x'] } },
        { id: 4, name: 'confluence', type: 'confluence', selected_tools: [] },
        { id: 5, type: 'slack', selected_tools: ['post'] },
      ],
    };
    expect(grantableTools(versionDetails)).toEqual([
      { toolkit: 'github', tool: 'delete_branch' },
      { toolkit: 'github', tool: 'create_pr' },
      { toolkit: 'jira', tool: 'delete_issue' },
    ]);
  });

  it('gives no tools for details without a tool list', () => {
    expect(grantableTools(undefined)).toEqual([]);
    expect(grantableTools({ tools: 'no' })).toEqual([]);
  });
});

describe('unattendedRefusalText', () => {
  it('names the tool and the reason', () => {
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'tool_not_resolved', field: 'sensitive_tools', tool: 'github/delete_repo' })).toBe(
      'The unattended grant was refused. The tool github/delete_repo is not one of the tools of this agent. Change the grant and send again.',
    );
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'tier_requires_hitl', tool: 'github/merge' })).toContain('The tier of this agent requires a person to approve the tool github/merge.');
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'disabled' })).toContain('An administrator turned off unattended grants.');
  });

  it('names the field of a value that is not valid, also for an unknown reason', () => {
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'invalid', field: 'chain.max_spend' })).toContain('The value of Most spend (USD) is not valid.');
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'over_maximum', field: 'chain.max_continuations' })).toContain(
      'Most automatic continuations: the value is more than the platform maximum. Type a lower value.',
    );
    expect(unattendedRefusalText({ error: 'unattended_grant_refused', reason: 'something_new' })).toContain('The value of something_new is not valid.');
  });

  it('ignores any other body', () => {
    expect(unattendedRefusalText({ error: 'unsupported_agent_execution', reason: 'x' })).toBeUndefined();
    expect(unattendedRefusalText(null)).toBeUndefined();
  });
});

import { describe, expect, it } from 'vitest';

import type { DelegationGrant, DelegationItem } from '@/shared/api/generated/model';

import { edgeKindFor, grantView, groupItems, groupWithheldReason, isWithheld, itemKey, itemsToApprove, toolkitNames, withheldReason } from './delegationReview';

const GIT: DelegationItem = { kind: 'toolkit', name: 'toolkit:12' };
const GIT_PUSH: DelegationItem = { kind: 'tool', name: 'push', toolkit: 'toolkit:12' };
const GIT_CREDENTIAL: DelegationItem = { kind: 'credential', name: 'ref-1', toolkit: 'toolkit:12' };
const DB: DelegationItem = { kind: 'toolkit', name: 'toolkit:13' };
const DB_ALL: DelegationItem = { kind: 'tool', name: '*', toolkit: 'toolkit:13' };
const CLASS: DelegationItem = { kind: 'data_class', name: 'internal' };
const HOST: DelegationItem = { kind: 'egress_host', name: 'git.example.com' };

function grant(overrides: Partial<DelegationGrant>): DelegationGrant {
  return { state: 'pending', governed: false, ...overrides } as DelegationGrant;
}

describe('groupItems', () => {
  it('groups tools and the credential reference under their toolkit and keeps classes and hosts apart', () => {
    const grouped = groupItems([GIT, GIT_PUSH, GIT_CREDENTIAL, DB, DB_ALL, CLASS, HOST], new Map([['toolkit:12', 'Git']]));

    expect(grouped.toolkits.map((group) => group.label)).toEqual(['Git', 'Toolkit 13']);
    expect(grouped.toolkits[0]?.tools).toEqual([GIT_PUSH]);
    expect(grouped.toolkits[0]?.credentials).toEqual([GIT_CREDENTIAL]);
    expect(grouped.toolkits[1]?.tools).toEqual([DB_ALL]);
    expect(grouped.dataClasses).toEqual([CLASS]);
    expect(grouped.egressHosts).toEqual([HOST]);
  });

  it('names a built-in toolkit by its type', () => {
    expect(groupItems([{ kind: 'toolkit', name: 'builtin:calculator' }]).toolkits[0]?.label).toBe('calculator');
  });
});

describe('toolkitNames', () => {
  it('maps toolkit ids to names and ignores attached agents', () => {
    const names = toolkitNames([
      { id: 1, tool_id: 12, name: 'Git', type: 'github' },
      { id: 2, tool_id: 99, name: 'Sub agent', type: 'application' },
      { id: 3, tool_id: 14, name: '  ', type: 'jira' },
    ]);
    expect([...names.entries()]).toEqual([['toolkit:12', 'Git']]);
  });
});

describe('what the approver holds', () => {
  it('names the credential as the reason when that is what is missing, else the toolkit', () => {
    const group = groupItems([GIT, GIT_CREDENTIAL]).toolkits[0];
    if (group === undefined) throw new Error('no group');
    const toolkitOnly = new Set([itemKey(GIT)]);
    const withCredential = new Set([itemKey(GIT), itemKey(GIT_CREDENTIAL)]);

    expect(isWithheld(group, toolkitOnly)).toBe(true);
    expect(isWithheld(group, new Set())).toBe(false);
    expect(groupWithheldReason(group, toolkitOnly)).toBe(withheldReason(GIT));
    expect(groupWithheldReason(group, withCredential)).toBe(withheldReason(GIT_CREDENTIAL));
    expect(withheldReason(GIT)).toMatch(/access to this toolkit/);
    expect(withheldReason(GIT_CREDENTIAL)).toMatch(/credential/);
  });

  it('sends nothing (approve all the approver holds) until a toolkit is unticked, then drops the toolkit with its tools and credential', () => {
    const approvable = [GIT, GIT_PUSH, GIT_CREDENTIAL, DB, DB_ALL, CLASS, HOST];

    expect(itemsToApprove(approvable, new Set())).toBeUndefined();
    expect(itemsToApprove(approvable, new Set(['toolkit:12']))).toEqual([DB, DB_ALL, CLASS, HOST]);
  });
});

describe('grantView', () => {
  it('reads the state of an edge: approved, awaiting a change request, pending, needs re-approval', () => {
    expect(grantView(grant({ state: 'approved' }))).toBe('approved');
    expect(grantView(grant({ state: 'stale' }))).toBe('stale');
    expect(grantView(grant({ state: 'pending', governed: true }))).toBe('awaiting');
    expect(grantView(grant({ state: 'pending' }))).toBe('pending');
  });

  it('makes a pipeline parent a pipeline child edge and any other an agent tool', () => {
    expect(edgeKindFor(true)).toBe('pipeline_child');
    expect(edgeKindFor(false)).toBe('agent_tool');
  });
});

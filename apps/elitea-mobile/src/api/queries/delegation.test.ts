import { DELEGATION_COPY, DELEGATION_CONTRACT, DelegationError, canApprove, canRevoke, delegationErrorText, grantOf, grantsOf, stateLabel } from '@/api/queries/delegation';

const grant = (id: number, extra: Record<string, unknown> = {}) => ({
  id,
  caller_application_id: 41,
  callee_application_id: 50 + id,
  edge_kind: 'agent_tool',
  state: 'pending',
  governed: false,
  legacy: false,
  items: [{ kind: 'toolkit', name: 'toolkit:7' }, { kind: 'tool', name: 'git_push', toolkit: 'toolkit:7' }],
  omitted: [],
  ...extra,
});

describe('grants of an answer', () => {
  it('reads the live grants and drops history and malformed rows', () => {
    const rows = grantsOf({
      grants: [grant(1), grant(2, { state: 'superseded' }), grant(3, { state: 'revoked' }), { id: 'x' }, grant(1), grant(4, { state: 'stale' })],
    });
    expect(rows.map((r) => r.id)).toEqual([1, 4]);
    expect(rows[0].items).toEqual([
      { kind: 'toolkit', name: 'toolkit:7', toolkit: null },
      { kind: 'tool', name: 'git_push', toolkit: 'toolkit:7' },
    ]);
  });

  it('refuses a body that is not a grant list and keeps a grant with a bad item list readable', () => {
    expect(() => grantsOf('nope')).toThrow(DelegationError);
    expect(grantOf(grant(1, { items: [{ kind: 1 }, { kind: 'egress_host', name: 'git.corp' }] }))?.items).toHaveLength(1);
    expect(grantOf({ id: 0 })).toBeNull();
  });

  it('carries the change request of a governed grant', () => {
    expect(grantOf(grant(1, { governed: true, change_request_id: 9 }))).toMatchObject({ governed: true, changeRequestId: 9 });
  });
});

describe('state', () => {
  it('names the state the way the web does', () => {
    expect(stateLabel({ state: 'approved' })).toBe('Approved');
    expect(stateLabel({ state: 'pending' })).toBe('Awaiting approval');
    expect(stateLabel({ state: 'stale' })).toBe('Needs re-approval');
  });

  it('offers Approve only on a pending grant that is not governed', () => {
    expect(canApprove({ state: 'pending', governed: false })).toBe(true);
    expect(canApprove({ state: 'pending', governed: true })).toBe(false);
    expect(canApprove({ state: 'approved', governed: false })).toBe(false);
    expect(canApprove({ state: 'stale', governed: false })).toBe(false);
  });

  it('offers Revoke on a live grant, but not on a governed one still awaiting its request', () => {
    expect(canRevoke({ state: 'approved', governed: true })).toBe(true);
    expect(canRevoke({ state: 'stale', governed: false })).toBe(true);
    expect(canRevoke({ state: 'pending', governed: true })).toBe(false);
  });
});

it('shows a failure in the error own words', () => {
  expect(delegationErrorText(new DelegationError('offline', DELEGATION_COPY.offline))).toBe(DELEGATION_COPY.offline);
  expect(delegationErrorText(new Error('x'))).toBe(DELEGATION_COPY.load);
  expect(DELEGATION_CONTRACT).toEqual({ major: 1, minor: 59 });
});

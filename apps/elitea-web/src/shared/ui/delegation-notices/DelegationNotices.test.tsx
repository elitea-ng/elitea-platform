import { describe, expect, it } from 'vitest';
import { screen, within } from '@testing-library/react';

import { parseDelegationNotice, type DelegationNotice } from '@elitea/client-core/stream';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { DelegationNotices, delegationNoticeSentence, delegationReviewHref } from './DelegationNotices';

const refusedRaw = {
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  code: 'DELEGATION_GRANT_MISSING',
  edge: { caller_id: 2, caller_name: 'Agent2', callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' },
  missing: [{ kind: 'toolkit', name: 'git' }],
};

function notice(extra: Record<string, unknown> = {}): DelegationNotice {
  const parsed = parseDelegationNotice({ ...refusedRaw, ...extra });
  if (parsed === undefined) throw new Error('not a notice');
  return parsed;
}

describe('delegation notice sentence', () => {
  it('names who could not use what from whom', () => {
    expect(delegationNoticeSentence(notice())).toBe('Agent1 could not use git from Agent2. This delegation is not approved.');
  });

  it('says "its caller" when the caller has no name', () => {
    const anonymous = notice({ edge: { callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' } });
    expect(delegationNoticeSentence(anonymous)).toBe('Agent1 could not use git from its caller. This delegation is not approved.');
  });

  it('lists several grants and counts the rest', () => {
    const many = notice({ missing: [{ kind: 'toolkit', name: 'git' }, { kind: 'tool', name: 'push', toolkit: 'jira' }], missing_total: 5 });
    expect(delegationNoticeSentence(many)).toBe('Agent1 could not use git, jira / push and 3 more from Agent2. This delegation is not approved.');
  });

  it('reads drift as used without the new toolkit', () => {
    const drift = notice({ kind: 'narrowed', reason: 'stale', missing: [{ kind: 'toolkit', name: 'Database' }] });
    expect(delegationNoticeSentence(drift)).toBe('Agent1 ran without Database. It was added after this delegation was approved.');
  });

  it('says the person lacks access for a principal refusal, and names the depth limit', () => {
    expect(delegationNoticeSentence(notice({ reason: 'principal' }))).toBe('Agent1 could not use git. You do not have access to it.');
    expect(delegationNoticeSentence(notice({ reason: 'depth', code: 'DELEGATION_DEPTH_EXCEEDED', missing: [], depth: 3, max_depth: 2 }))).toBe(
      'Agent1 was not called: the delegation depth limit of 2 was reached.',
    );
  });
});

describe('DelegationNotices', () => {
  it('renders nothing without notices', () => {
    renderWithTheme(<DelegationNotices notices={[]} />);
    expect(screen.queryByTestId('delegation-notices')).toBeNull();
  });

  it('offers Review delegation to an editor of the caller, linking to the caller agent', () => {
    renderWithTheme(<DelegationNotices notices={[notice({ can_review: true })]} />);
    const card = screen.getByTestId('delegation-notice');
    expect(card).toHaveTextContent('Delegation refused');
    const link = within(card).getByRole('link', { name: 'Review delegation' });
    expect(link).toHaveAttribute('href', delegationReviewHref('', 2, 1));
    // The link names the called agent too, so the editor opens that edge's review dialog.
    expect(link.getAttribute('href')).toMatch(/\/agents\/all\/2\?review_delegation=1$/);
  });

  it('sends a refused pipeline node to the pipeline editor, naming no edge', () => {
    const node = notice({ can_review: true, edge: { ...refusedRaw.edge, kind: 'pipeline_node' } });
    renderWithTheme(<DelegationNotices notices={[node]} />);
    expect(screen.getByRole('link', { name: 'Review delegation' }).getAttribute('href')).toMatch(/\/pipelines\/all\/2$/);
  });

  it('links to the plain editor when the notice does not name the called agent', () => {
    expect(delegationReviewHref('', 2)).toBe('/agents/all/2');
    expect(delegationReviewHref('/app/', 2, 1)).toBe('/app/agents/all/2?review_delegation=1');
  });

  it('tells a viewer who may not review who to ask, with no action', () => {
    renderWithTheme(<DelegationNotices notices={[notice({ can_review: false, editors: ['Ada', 'Grace'] })]} />);
    expect(screen.queryByRole('link', { name: 'Review delegation' })).toBeNull();
    expect(screen.getByTestId('delegation-editors')).toHaveTextContent('Ask Ada, Grace to review this delegation.');
  });

  it('never offers the action for a principal or depth refusal, even to an editor', () => {
    renderWithTheme(
      <DelegationNotices
        notices={[
          notice({ id: 'a', reason: 'principal', can_review: true, editors: ['Ada'] }),
          notice({ id: 'b', reason: 'depth', can_review: true, editors: ['Ada'], missing: [] }),
        ]}
      />,
    );
    expect(screen.queryByRole('link', { name: 'Review delegation' })).toBeNull();
    expect(screen.queryByTestId('delegation-editors')).toBeNull();
    expect(screen.getAllByTestId('delegation-notice')).toHaveLength(2);
  });

  it('shows a notice without the action when the server did not say what the viewer may do', () => {
    renderWithTheme(<DelegationNotices notices={[notice()]} />);
    expect(screen.getByTestId('delegation-notice')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Review delegation' })).toBeNull();
  });

  it('renders untrusted names as plain text', () => {
    renderWithTheme(<DelegationNotices notices={[notice({ edge: { caller_id: 2, callee_name: '<img src=x onerror=alert(1)>', kind: 'agent_tool' } })]} />);
    expect(screen.getByTestId('delegation-notice').querySelector('img')).toBeNull();
  });
});

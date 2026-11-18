import { fireEvent, render, screen } from '@testing-library/react-native';
import { router } from 'expo-router';

import { frameOf } from '@/features/chat/__fixtures__/clientFrames';
import { DelegationNoticeCards } from '@/features/chat/DelegationNoticeCards';
import { delegationNoticeText, delegationReviewHref, storedDelegationNotices } from '@/features/chat/delegationNotices';
import { toChatMessages } from '@/features/chat/messageModel';
import { INITIAL_TURN, reduceTurn } from '@/features/chat/turnReducer';

jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));

const notice = (extra: Record<string, unknown> = {}) => ({
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  edge: { callee_name: 'Reviewer', caller_name: 'Planner', caller_id: 7, callee_id: 9, kind: 'agent_tool' },
  missing: [{ kind: 'toolkit', name: 'Git' }],
  missing_total: 1,
  can_review: true,
  editors: [],
  ...extra,
});

const frame = (type: string, n: Record<string, unknown>) => frameOf({ type, response_metadata: { delegation_v1: n } });

const parsed = (n: Record<string, unknown>) => storedDelegationNotices({ delegation_notices_v1: [n] })[0]!;

describe('delegation notices (client contract 1.60)', () => {
  it('folds both frame types without touching text or tools, and a replay does not double it', () => {
    let turn = reduceTurn(INITIAL_TURN, frame('delegation_refused', notice()));
    turn = reduceTurn(turn, frame('delegation_narrowed', notice({ id: 'call-2', kind: 'narrowed', reason: 'stale' })));
    turn = reduceTurn(turn, frame('delegation_refused', notice()));
    expect(turn.delegationNotices.map((n) => [n.id, n.kind])).toEqual([
      ['call-1', 'refused'],
      ['call-2', 'narrowed'],
    ]);
    expect(turn.content).toBe('');
    expect(turn.tools).toEqual([]);
    expect(turn.unknownFrames).toBe(0);
  });

  it('ignores a malformed notice', () => {
    const turn = reduceTurn(INITIAL_TURN, frame('delegation_refused', { id: 'x', kind: 'bogus' }));
    expect(turn.delegationNotices).toEqual([]);
  });

  it('reads a reloaded transcript the same as live', () => {
    const detail = {
      participants: [{ id: 1, entity_name: 'agent' }],
      message_groups: [{ id: 5, author_participant_id: 1, meta: { delegation_notices_v1: [notice(), { bad: true }] } }],
    };
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const [message] = toChatMessages(detail as any);
    const live = reduceTurn(INITIAL_TURN, frame('delegation_refused', notice())).delegationNotices;
    expect(message?.delegationNotices).toEqual(live);
  });

  it('words each reason', () => {
    expect(delegationNoticeText(parsed(notice()))).toBe('Reviewer could not use Git from Planner. This delegation is not approved.');
    expect(delegationNoticeText(parsed(notice({ edge: { callee_name: 'Reviewer', kind: 'agent_tool' }, missing_total: 3 })))).toBe(
      'Reviewer could not use Git and 2 more from its caller. This delegation is not approved.',
    );
    expect(delegationNoticeText(parsed(notice({ kind: 'narrowed', reason: 'stale' })))).toBe(
      'Reviewer ran without Git. It was added after this delegation was approved.',
    );
    expect(delegationNoticeText(parsed(notice({ reason: 'principal' })))).toBe('Reviewer could not use Git. You do not have access to it.');
    expect(delegationNoticeText(parsed(notice({ reason: 'depth', max_depth: 3 })))).toBe(
      'Reviewer was not called: the delegation depth limit (3) was reached.',
    );
  });

  it('shows Review delegation only when offered, and asks the editors otherwise', async () => {
    const { rerender } = await render(<DelegationNoticeCards notices={[parsed(notice())]} />);
    await fireEvent.press(screen.getByText('Review delegation'));
    // The notice names the called agent (9) and the caller (Planner), so the screen lands on that edge.
    expect(router.push).toHaveBeenCalledWith('/agent-delegations/7?callee=9&name=Planner');
    expect(delegationReviewHref(7)).toBe('/agent-delegations/7');

    await rerender(<DelegationNoticeCards notices={[parsed(notice({ reason: 'principal' }))]} />);
    expect(screen.queryByText('Review delegation')).toBeNull();

    await rerender(<DelegationNoticeCards notices={[parsed(notice({ can_review: false, editors: ['Ana', 'Bo'] }))]} />);
    expect(screen.queryByText('Review delegation')).toBeNull();
    expect(screen.getByText('Ask Ana, Bo to review this delegation.')).toBeTruthy();

    await rerender(<DelegationNoticeCards notices={[parsed(notice({ can_review: false }))]} />);
    expect(screen.queryByText(/^Ask /)).toBeNull();
  });
});

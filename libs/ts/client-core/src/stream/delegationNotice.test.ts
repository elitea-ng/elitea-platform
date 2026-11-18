import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  delegationNoticeOf,
  foldDelegationNotice,
  missingNames,
  MAX_DELEGATION_NOTICES,
  offersReview,
  parseDelegationNotice,
  parseDelegationNotices,
  type DelegationNotice,
} from './delegationNotice';

const refused = {
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  code: 'DELEGATION_GRANT_MISSING',
  edge: { caller_id: 2, caller_name: 'Agent2', callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' },
  missing: [{ kind: 'toolkit', name: 'Git' }],
  can_review: true,
};

function parsed(raw: unknown): DelegationNotice {
  const notice = parseDelegationNotice(raw);
  assert.ok(notice, 'a notice was expected');
  return notice;
}

describe('parseDelegationNotice', () => {
  it('reads a refusal into one shape', () => {
    const notice = parsed(refused);
    assert.equal(notice.id, 'call-1');
    assert.equal(notice.calleeName, 'Agent1');
    assert.equal(notice.callerName, 'Agent2');
    assert.equal(notice.callerId, 2);
    assert.deepEqual(notice.missing, [{ kind: 'toolkit', name: 'Git' }]);
    assert.equal(notice.canReview, true);
    assert.deepEqual(notice.editors, []);
  });

  it('drops what is not a notice', () => {
    for (const bad of [
      null,
      'x',
      [],
      { ...refused, id: '' },
      { ...refused, kind: 'other' },
      { ...refused, reason: 'because' },
      { ...refused, edge: { kind: 'agent_tool' } },
    ]) {
      assert.equal(parseDelegationNotice(bad), undefined, JSON.stringify(bad));
    }
  });

  it('bounds the lists and keeps only known grant kinds', () => {
    const many = Array.from({ length: 40 }, (_, index) => ({ kind: 'toolkit', name: `T${String(index)}` }));
    const notice = parsed({ ...refused, missing: [...many, { kind: 'secret', name: 'x' }], missing_total: 40, editors: ['A', 'B', 'C', 'D'] });
    assert.equal(notice.missing.length, 16);
    assert.equal(notice.missingTotal, 40);
    assert.equal(notice.editors.length, 3);
    assert.equal(parsed({ ...refused, missing: [{ kind: 'secret', name: 'x' }] }).missing.length, 0);
  });

  it('does not invent can_review', () => {
    const { can_review: _omit, ...unstamped } = refused;
    assert.equal(parsed(unstamped).canReview, undefined);
    assert.equal(offersReview(parsed(unstamped)), false);
  });
});

describe('offersReview', () => {
  it('offers Review delegation to an editor of a grant-missing or stale delegation', () => {
    assert.equal(offersReview(parsed(refused)), true);
    assert.equal(offersReview(parsed({ ...refused, kind: 'narrowed', reason: 'stale' })), true);
  });

  it('never offers it for a principal or depth refusal, a viewer who may not, or an unsaved caller', () => {
    assert.equal(offersReview(parsed({ ...refused, reason: 'principal' })), false);
    assert.equal(offersReview(parsed({ ...refused, reason: 'depth' })), false);
    assert.equal(offersReview(parsed({ ...refused, can_review: false, editors: ['Ada'] })), false);
    assert.equal(offersReview(parsed({ ...refused, edge: { callee_name: 'Agent1', kind: 'agent_tool' } })), false);
  });
});

describe('folding', () => {
  it('keeps one notice per call, in order, bounded', () => {
    let notices: readonly DelegationNotice[] = [];
    notices = foldDelegationNotice(notices, parsed(refused));
    notices = foldDelegationNotice(notices, parsed({ ...refused, reason: 'principal' }));
    assert.equal(notices.length, 1);
    assert.equal(notices[0]?.reason, 'principal');
    for (let index = 0; index < 30; index += 1) {
      notices = foldDelegationNotice(notices, parsed({ ...refused, id: `call-${String(index + 10)}` }));
    }
    assert.equal(notices.length, MAX_DELEGATION_NOTICES);
  });

  it('reads a settled answer and a live frame the same way', () => {
    assert.deepEqual(parseDelegationNotices([refused, { id: 'x' }, 'junk']), [parsed(refused)]);
    assert.deepEqual(delegationNoticeOf({ delegation_v1: refused }), parsed(refused));
    assert.equal(delegationNoticeOf({}), undefined);
    assert.equal(delegationNoticeOf(null), undefined);
  });

  it('names a tool with its toolkit', () => {
    const notice = parsed({ ...refused, missing: [{ kind: 'toolkit', name: 'Git' }, { kind: 'tool', name: 'push', toolkit: 'Git' }] });
    assert.deepEqual(missingNames(notice), ['Git', 'Git / push']);
  });
});

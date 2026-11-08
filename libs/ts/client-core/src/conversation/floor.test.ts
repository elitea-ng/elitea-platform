import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { DEFAULT_FLOOR, MAX_ADDRESSED, addressedTo, floorOf, foldSideTalk, metaWithLead, sideTalkLabel } from './floor';
import { MESSAGE_CLASS, type MessageClass } from './messageClass';

describe('floorOf', () => {
  it('reads a lead floor', () => {
    assert.deepEqual(floorOf({ floor: { policy: 'lead', lead_participant_id: 7 } }), { policy: 'lead', leadParticipantId: 7 });
  });
  it('reads anything else as the default', () => {
    for (const meta of [null, undefined, 3, {}, { floor: 'lead' }, { floor: { policy: 'lead' } }, { floor: { policy: 'lead', lead_participant_id: -1 } }, { floor: { policy: 'addressed', lead_participant_id: 7 } }]) {
      assert.deepEqual(floorOf(meta), DEFAULT_FLOOR);
    }
  });
});

describe('metaWithLead', () => {
  it('keeps the other keys and sets or clears the floor', () => {
    const meta = { steps_limit: 9, floor: { policy: 'lead', lead_participant_id: 2 } };
    assert.deepEqual(metaWithLead(meta, 5), { steps_limit: 9, floor: { policy: 'lead', lead_participant_id: 5 } });
    assert.deepEqual(metaWithLead(meta, 0), { steps_limit: 9 });
    assert.deepEqual(meta.floor.lead_participant_id, 2);
    assert.deepEqual(metaWithLead(null, 3), { floor: { policy: 'lead', lead_participant_id: 3 } });
  });
});

describe('addressedTo', () => {
  const a = { participantId: 21, name: 'Analyst' };
  const r = { participantId: 22, name: 'Reviewer' };
  it('keeps the chosen agents the text still mentions, in the order chosen', () => {
    assert.deepEqual(addressedTo('@Reviewer and #Analyst please', [r, a]), [22, 21]);
    assert.deepEqual(addressedTo('only @Analyst', [r, a]), [21]);
    assert.deepEqual(addressedTo('no one', [r, a]), []);
  });
  it('matches a whole name only', () => {
    assert.deepEqual(addressedTo('@Analysts', [a]), []);
    assert.deepEqual(addressedTo('mail@Analyst', [a]), []);
    assert.deepEqual(addressedTo('(@Analyst)', [a]), [21]);
  });
  it('deduplicates, skips bad ids and caps', () => {
    assert.deepEqual(addressedTo('@Analyst', [a, a, { participantId: 0, name: 'Analyst' }]), [21]);
    const many = Array.from({ length: MAX_ADDRESSED + 3 }, (_, i) => ({ participantId: i + 1, name: `A${i}` }));
    assert.equal(addressedTo(many.map((x) => `@${x.name}`).join(' '), many).length, MAX_ADDRESSED);
  });
});

describe('foldSideTalk', () => {
  const row = (id: string, messageClass: MessageClass) => ({ id, messageClass });
  it('folds each run of agent-to-agent rows', () => {
    const rows = [
      row('1', MESSAGE_CLASS.Human),
      row('2', MESSAGE_CLASS.AgentToAgent),
      row('3', MESSAGE_CLASS.AgentToAgent),
      row('4', MESSAGE_CLASS.AgentToHumans),
      row('5', MESSAGE_CLASS.AgentToAgent),
    ];
    const folded = foldSideTalk(rows, (r) => r.id);
    assert.deepEqual(
      folded.map((e) => (e.kind === 'row' ? e.row.id : `${e.key}x${e.rows.length}`)),
      ['1', 'side:2x2', '4', 'side:5x1'],
    );
  });
});

describe('sideTalkLabel', () => {
  it('names the author and addressees', () => {
    assert.equal(sideTalkLabel('Analyst', ['Reviewer', '']), 'Analyst → Reviewer');
    assert.equal(sideTalkLabel('Analyst', []), 'Analyst');
  });
});

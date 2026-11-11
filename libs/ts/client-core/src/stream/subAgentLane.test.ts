import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { foldSubAgentEntry, parseSubAgentLanes, settleSubAgentLanes, subAgentEntryOf, type CloudSubAgentLane } from './subAgentLane';

const started = { kind: 'started', agent_id: 'a_1', parent_call_id: 'call-spawn', ordinal: 1, label: 'callers', display_name: 'Owl', avatar: 'default:owl' };

function fold(entries: unknown[]): readonly CloudSubAgentLane[] {
  return entries.reduce<readonly CloudSubAgentLane[]>((lanes, entry) => foldSubAgentEntry(lanes, entry), []);
}

/** The lane at `index`, failing the test when there is none. */
function laneAt(lanes: readonly CloudSubAgentLane[], index = 0): CloudSubAgentLane {
  const lane = lanes[index];
  assert.ok(lane, `no lane ${String(index)}`);
  return lane;
}

describe('foldSubAgentEntry', () => {
  it('folds a child from its start to its end', () => {
    const lanes = fold([
      started,
      { kind: 'tool_start', agent_id: 'a_1', call_id: 'c1', tool: 'read_file', args_summary: 'src/main.rs' },
      { kind: 'tool_end', agent_id: 'a_1', call_id: 'c1', ok: true, summary: 'fn main' },
      { kind: 'text', agent_id: 'a_1', text: 'Two callers.' },
      { kind: 'finished', agent_id: 'a_1', status: 'completed', steps: 2, input_tokens: 10, output_tokens: 3, result: 'Two callers.' },
    ]);
    assert.equal(lanes.length, 1);
    const lane = laneAt(lanes);
    assert.equal(lane.displayName, 'Owl');
    assert.equal(lane.avatar, 'default:owl');
    assert.equal(lane.status, 'completed');
    assert.deepEqual(lane.items, [
      { type: 'tool', callId: 'c1', tool: 'read_file', argsSummary: 'src/main.rs', ok: true, summary: 'fn main' },
      { type: 'text', text: 'Two callers.' },
    ]);
    assert.equal(lane.result, 'Two callers.');
    assert.equal(lane.steps, 2);
  });

  it('keeps the same array for an entry that does not apply', () => {
    const lanes = fold([started]);
    for (const entry of [
      null,
      { kind: 'text', agent_id: 'other', text: 'x' },
      { kind: 'nope', agent_id: 'a_1' },
      { kind: 'tool_end', agent_id: 'a_1', call_id: 'missing', ok: true },
      { kind: 'finished', agent_id: 'a_1', status: 'exploded' },
      started,
    ]) {
      assert.equal(foldSubAgentEntry(lanes, entry), lanes);
    }
  });

  it('drops control characters and bounds text', () => {
    const lanes = fold([started, { kind: 'text', agent_id: 'a_1', text: `a\u0007b${'x'.repeat(2000)}` }]);
    const item = laneAt(lanes).items[0];
    assert.ok(item?.type === 'text' && item.text.startsWith('ab') && [...item.text].length === 600);
  });

  it('a continued child runs again', () => {
    const lanes = fold([started, { kind: 'finished', agent_id: 'a_1', status: 'completed', result: 'r' }, { kind: 'resumed', agent_id: 'a_1' }]);
    assert.equal(laneAt(lanes).status, 'running');
    assert.equal(laneAt(lanes).result, undefined);
  });
});

describe('parseSubAgentLanes', () => {
  it('reads the stored lanes and skips garbage', () => {
    const lanes = parseSubAgentLanes([
      {
        agent_id: 'a_1',
        parent_call_id: 'p',
        ordinal: 1,
        display_name: 'Owl',
        status: 'failed',
        items: [{ type: 'tool', call_id: 'c', tool: 't', ok: false }, { type: 'x' }],
        error: 'subagent_failed',
      },
      { agent_id: '', parent_call_id: 'p', ordinal: 2 },
      'nope',
    ]);
    assert.equal(lanes.length, 1);
    assert.equal(laneAt(lanes).status, 'failed');
    assert.deepEqual(laneAt(lanes).items, [{ type: 'tool', callId: 'c', tool: 't', ok: false }]);
    assert.deepEqual(parseSubAgentLanes({}), []);
  });

  it('a settled answer stops a lane left running', () => {
    const lanes = fold([started]);
    assert.equal(laneAt(settleSubAgentLanes(lanes)).status, 'cancelled');
    const done = fold([started, { kind: 'finished', agent_id: 'a_1', status: 'completed' }]);
    assert.equal(settleSubAgentLanes(done), done);
  });

  it('subAgentEntryOf reads the frame entry', () => {
    assert.deepEqual(subAgentEntryOf({ type: 'agent_subagent', response_metadata: { subagent_v1: started } }), started);
    assert.equal(subAgentEntryOf({ type: 'agent_subagent' }), undefined);
  });
});

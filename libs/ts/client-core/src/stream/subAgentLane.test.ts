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

  // #1872: a depth-2 start names its parent; the fold nests it under the
  // parent's children instead of adding a top-level lane.
  it('nests a depth-2 start under its parent', () => {
    const grandchild = { kind: 'started', agent_id: 'a_2', parent_call_id: 'call-nested', ordinal: 2, depth: 2, parent_agent_id: 'a_1' };
    const lanes = fold([started, grandchild, { kind: 'text', agent_id: 'a_2', text: 'from the grandchild' }]);
    assert.equal(lanes.length, 1, 'the grandchild must not be a top-level lane');
    const parent = laneAt(lanes);
    assert.equal(parent.children?.length, 1);
    const child = parent.children?.[0];
    assert.equal(child?.agentId, 'a_2');
    assert.equal(child?.depth, 2);
    assert.equal(child?.parentAgentId, 'a_1');
    assert.deepEqual(child?.items, [{ type: 'text', text: 'from the grandchild' }]);
  });

  it('a depth-2 start whose named parent is not among the lanes stays at the top level', () => {
    const orphan = { kind: 'started', agent_id: 'a_2', parent_call_id: 'call-nested', ordinal: 1, depth: 2, parent_agent_id: 'a_missing' };
    const lanes = fold([orphan]);
    assert.equal(lanes.length, 1);
    assert.equal(laneAt(lanes).depth, 1, 'without its parent, a depth-2 start is not trusted as depth 2');
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

  // #1872: Main's fold already nests a depth-2 lane under its parent's
  // `children` on read; this reader carries that shape through rather than
  // re-nesting, and settles a running grandchild too.
  it('reads nested children and settles them', () => {
    const lanes = parseSubAgentLanes([
      {
        agent_id: 'a_1',
        parent_call_id: 'p',
        ordinal: 1,
        status: 'completed',
        items: [],
        children: [{ agent_id: 'a_2', parent_call_id: 'p2', ordinal: 2, depth: 2, parent_agent_id: 'a_1', status: 'running', items: [] }],
      },
    ]);
    const child = laneAt(lanes).children?.[0];
    assert.equal(child?.agentId, 'a_2');
    assert.equal(child?.depth, 2);
    assert.equal(child?.parentAgentId, 'a_1');
    const settled = settleSubAgentLanes(lanes);
    assert.equal(laneAt(settled).children?.[0]?.status, 'cancelled');
  });

  it('nests a depth-2 lane only under a top-level depth-1 parent', () => {
    let lanes: readonly CloudSubAgentLane[] = [];
    lanes = foldSubAgentEntry(lanes, { kind: 'started', agent_id: 'a_1', parent_call_id: 'p', ordinal: 1 });
    lanes = foldSubAgentEntry(lanes, { kind: 'started', agent_id: 'a_2', parent_call_id: 'p', ordinal: 2, depth: 2, parent_agent_id: 'a_1' });
    // a_3 names a depth-2 lane as its parent: kept top-level, never depth 3.
    lanes = foldSubAgentEntry(lanes, { kind: 'started', agent_id: 'a_3', parent_call_id: 'p', ordinal: 3, depth: 2, parent_agent_id: 'a_2' });
    assert.deepEqual(lanes.map((l) => l.agentId), ['a_1', 'a_3']);
    assert.deepEqual(laneAt(lanes).children?.map((l) => l.agentId), ['a_2']);
    assert.equal(laneAt(lanes).children?.[0]?.children, undefined);
  });

  it('parseSubAgentLanes reads no deeper than one level of children', () => {
    const grand = { agent_id: 'a_3', parent_call_id: 'p', ordinal: 3, depth: 2, parent_agent_id: 'a_2', status: 'running', items: [] };
    const lanes = parseSubAgentLanes([
      { agent_id: 'a_1', parent_call_id: 'p', ordinal: 1, status: 'completed', items: [], children: [{ agent_id: 'a_2', parent_call_id: 'p', ordinal: 2, depth: 2, parent_agent_id: 'a_1', status: 'running', items: [], children: [grand] }] },
    ]);
    assert.equal(laneAt(lanes).children?.[0]?.children, undefined);
  });
});

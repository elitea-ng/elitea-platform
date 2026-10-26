import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  ANSWER_DELTA_TYPES,
  EXECUTION_EVENT,
  FRAME_TYPE,
  PAUSE_FINISH_REASONS,
  TOOL_CALL_TYPES,
  TURN_FAILURE_TYPES,
  parseEventData,
} from './frameTypes';

describe('FRAME_TYPE', () => {
  it('has no two members with the same wire name', () => {
    const values = Object.values(FRAME_TYPE);
    assert.equal(new Set(values).size, values.length);
  });

  it('every classification set names only vocabulary members', () => {
    const vocabulary = new Set<string>(Object.values(FRAME_TYPE));
    for (const set of [ANSWER_DELTA_TYPES, TURN_FAILURE_TYPES, TOOL_CALL_TYPES]) {
      for (const type of set) assert.ok(vocabulary.has(type), type);
    }
  });

  it('a failure, a delta and a tool frame are disjoint classes', () => {
    for (const type of ANSWER_DELTA_TYPES) assert.ok(!TURN_FAILURE_TYPES.has(type) && !TOOL_CALL_TYPES.has(type), type);
    for (const type of TURN_FAILURE_TYPES) assert.ok(!TOOL_CALL_TYPES.has(type), type);
  });

  it('the pause reasons are the contract 1.2 set', () => {
    assert.deepEqual([...PAUSE_FINISH_REASONS].sort(), ['awaiting_approval', 'awaiting_input', 'interrupted']);
  });

  it('the execution event names are the replay stream names', () => {
    assert.deepEqual(EXECUTION_EVENT, {
      node: 'execution.node_event',
      failed: 'execution.failed',
      replayReset: 'execution.replay_reset',
      indexIngestCompleted: 'index.ingest.completed',
      steerQueued: 'steer.queued',
      steerInjected: 'steer.injected',
      steerDropped: 'steer.dropped',
    });
  });
});

describe('parseEventData', () => {
  it('returns the object of valid JSON', () => {
    assert.deepEqual(parseEventData('{"type":"chunk","content":"a"}'), { type: 'chunk', content: 'a' });
  });

  for (const data of ['', 'not json', '[1]', 'null', '"text"', '42', undefined, 7]) {
    it(`returns undefined for ${JSON.stringify(data) ?? String(data)}`, () => {
      assert.equal(parseEventData(data), undefined);
    });
  }
});

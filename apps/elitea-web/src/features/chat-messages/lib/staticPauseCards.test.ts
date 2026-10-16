import { describe, expect, it } from 'vitest';

import { buildStaticPauseCards } from './staticPauseCards';
import { staticPauseBinding } from './staticPipelinePause';

const response = '30e0913e-10d4-43db-b8d0-c7b79480935a';
const generation = 'ee92ccbd-3312-4c72-b20b-fddf224e7c0e';
const proof = (letter: string, node = 'tick') => ({
  revision: 1,
  pause_id: `pipeline-static:sha256:${letter.repeat(64)}`,
  checkpoint_id: 'original-checkpoint',
  kind: 'before' as const,
  node_name: node,
  definition_digest: `sha256:${'b'.repeat(64)}`,
  node_digest: `sha256:${'c'.repeat(64)}`,
  pending_nodes: [node],
  step: 7,
  descendant_path: [],
});
const leaf = (call: string, child: string, letter: string, ordinal: number) => ({
  tool_call_id: call,
  child_thread_id: child,
  original_batch_event_id: `batch-${letter}`,
  original_ordinal: ordinal,
  proof: proof(letter),
});
// The saved pipeline's delegation wrapper, as the Worker's tool entry stamps it.
const wrapper = (id: string, name: string) => ({
  id,
  type: 'tool',
  name,
  original_name: name,
  parent_agent_call_id: id,
  status: 'complete',
  toolMeta: { toolkit_type: 'pipeline', agent_type: 'pipeline' },
});

describe('buildStaticPauseCards', () => {
  it('puts a root pipeline pause on the coordinator with its node name only', () => {
    const pause = staticPauseBinding(response, generation, 'root', proof('a'), undefined);
    const { cards, buckets } = buildStaticPauseCards(pause);
    expect(cards).toEqual([{ key: proof('a').pause_id, instanceKey: '', nodeName: 'tick', pauseId: undefined }]);
    expect(buckets).toEqual([]);
  });

  it('places each saved-pipeline leaf in its spawning call bucket, numbered like any sibling', () => {
    const pause = staticPauseBinding(response, generation, 'root', undefined, {
      revision: 1,
      pauses: [leaf('call-1', 'child-1', 'a', 1), leaf('call-2', 'child-2', 'd', 2)],
    });
    const { cards, buckets } = buildStaticPauseCards(pause, [wrapper('call-1', 'Reviewer'), wrapper('call-2', 'Reviewer')]);
    expect(cards.map((card) => [card.instanceKey, card.pauseId === proof(card.key.slice(-1)).pause_id])).toEqual([
      ['call-1', true],
      ['call-2', true],
    ]);
    expect(buckets.map((bucket) => bucket.label)).toEqual(['Reviewer (1)', 'Reviewer (2)']);
  });

  it('never exposes the child thread id as a label when the wrapper is unknown', () => {
    const pause = staticPauseBinding(response, generation, 'root', undefined, { revision: 1, pauses: [leaf('call-9', 'child-9', 'e', 1)] });
    const { buckets } = buildStaticPauseCards(pause, [], 'Pipeline');
    expect(buckets).toEqual([{ instanceKey: 'call-9', name: 'Pipeline', label: 'Pipeline', agentType: 'pipeline' }]);
  });
});

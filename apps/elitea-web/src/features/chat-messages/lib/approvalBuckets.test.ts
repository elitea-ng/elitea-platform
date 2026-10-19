import { describe, expect, it } from 'vitest';

import { buildHitlBuckets, groupToolkitAuthorizationActions } from './approvalBuckets';
import { normalizeHitlInterrupt } from './hitlInterrupts';

/** A pending card exactly as the Worker overlays a nested pause (`events.rs` overlay_hierarchy_object). */
function card(id: string, path: readonly { name: string; call_id: string; sibling_ordinal?: number }[], extra: Record<string, unknown> = {}) {
  const owner = path[path.length - 1];
  return normalizeHitlInterrupt({
    type: 'hitl',
    interrupt_id: id,
    guardrail_type: 'sensitive_tool',
    available_actions: ['approve', 'reject', 'block_with_comment'],
    tool_call_id: `tc-${id}`,
    tool_name: 'delete_file',
    ...(owner ? { parent_agent_path: path, parent_agent_name: owner.name, parent_agent_call_id: owner.call_id } : {}),
    ...extra,
  });
}

describe('buildHitlBuckets', () => {
  it('keeps a root sensitive-tool card on the coordinator, without a bucket', () => {
    const buckets = buildHitlBuckets([card('i1', [])]);
    expect(buckets.coordinator.map((entry) => entry.interrupt.interrupt_id)).toEqual(['i1']);
    expect(buckets.hasSubAgents).toBe(false);
  });

  it('labels a pipeline → saved agent pause `Pipeline ▸ Agent`', () => {
    const buckets = buildHitlBuckets([card('i1', [{ name: 'Pipeline', call_id: 'p1' }, { name: 'Agent', call_id: 'a1' }])]);
    expect(buckets.subAgents).toHaveLength(1);
    expect(buckets.subAgents[0]).toMatchObject({ instanceKey: 'a1', name: 'Agent', label: 'Pipeline ▸ Agent' });
  });

  it('gives two parallel calls of one sub-agent two buckets, `Sub (1)` and `Sub (2)`', () => {
    const buckets = buildHitlBuckets([
      card('i2', [{ name: 'Sub', call_id: 'call-b', sibling_ordinal: 2 }]),
      card('i1', [{ name: 'Sub', call_id: 'call-a', sibling_ordinal: 1 }]),
    ]);
    expect(buckets.subAgents.map((bucket) => [bucket.instanceKey, bucket.label, bucket.entries.length])).toEqual([
      ['call-b', 'Sub (2)', 1],
      ['call-a', 'Sub (1)', 1],
    ]);
  });

  it('numbers only the repeated root when the same sub-agent pauses under two parents', () => {
    const buckets = buildHitlBuckets([
      card('i1', [{ name: 'Parent', call_id: 'p1', sibling_ordinal: 1 }, { name: 'Sub', call_id: 's1' }]),
      card('i2', [{ name: 'Parent', call_id: 'p2', sibling_ordinal: 2 }, { name: 'Sub', call_id: 's2' }]),
    ]);
    expect(buckets.subAgents.map((bucket) => bucket.label)).toEqual(['Parent (1) ▸ Sub', 'Parent (2) ▸ Sub']);
  });

  it('keeps `(2)` for the lone still-paused sibling from the trace context', () => {
    const trace = [
      { type: 'tool', name: 'x', parent_agent_name: 'Sub', parent_agent_call_id: 'call-a', parent_agent_path: [{ name: 'Sub', call_id: 'call-a', sibling_ordinal: 1 }] },
    ];
    const buckets = buildHitlBuckets([card('i2', [{ name: 'Sub', call_id: 'call-b', sibling_ordinal: 2 }])], trace);
    expect(buckets.subAgents[0]?.label).toBe('Sub (2)');
  });

  it('puts two cards of one instance into one bucket and never folds two instances by name', () => {
    const path = [{ name: 'Sub', call_id: 'call-a' }];
    const buckets = buildHitlBuckets([card('i1', path), card('i2', path), card('i3', [{ name: 'Sub', call_id: 'call-b' }])]);
    expect(buckets.subAgents.map((bucket) => bucket.entries.map((entry) => entry.interrupt.interrupt_id))).toEqual([['i1', 'i2'], ['i3']]);
  });

  it('separates mixed coordinator and sub-agent cards and resolves the agent type', () => {
    const buckets = buildHitlBuckets(
      [card('root', []), card('ask', [{ name: 'Researcher', call_id: 'r1' }], { guardrail_type: 'clarifying_question', available_actions: ['answer'] })],
      [],
      { Researcher: 'application' },
    );
    expect(buckets.coordinator).toHaveLength(1);
    expect(buckets.subAgents[0]).toMatchObject({ label: 'Researcher', agentType: 'application' });
  });
});

describe('groupToolkitAuthorizationActions', () => {
  const direct = { id: 'direct-call', name: 'list_direct', status: 'action_required', toolMeta: { server_url: 'https://direct.example' } };
  const nested = {
    id: 'nested-call',
    name: 'list_nested',
    status: 'action_required',
    parent_agent_name: 'Leaf',
    parent_agent_call_id: 'leaf-call',
    parent_agent_path: [
      { name: 'Orchestrator', call_id: 'orchestrator-call' },
      { name: 'Suborchestrator', call_id: 'suborchestrator-call' },
      { name: 'Leaf', call_id: 'leaf-call' },
    ],
    toolMeta: {},
  };

  // Reference golden: mcpAuthorization.helpers.test.js:154-201.
  it('keeps direct cards flat and groups delegated authorization by full origin path', () => {
    const grouped = groupToolkitAuthorizationActions([direct, nested], [direct, nested]);
    expect(grouped.coordinator).toEqual([direct]);
    expect(grouped.subAgents).toHaveLength(1);
    expect(grouped.subAgents[0]).toMatchObject({ name: 'Leaf', label: 'Orchestrator ▸ Suborchestrator ▸ Leaf', actions: [nested] });
  });
});

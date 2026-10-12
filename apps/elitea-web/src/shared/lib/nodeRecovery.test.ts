import { describe, expect, it } from 'vitest';
import receipt from './fixtures/node-recovery-required.json';
import { nodeRecoveryBinding, nodeRecoveryFromEvent } from './nodeRecovery';

const response = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const frame = { type: 'agent_node_recovery_required', message_id: response, execution_generation: '1',
  response_metadata: { node_recovery_required_v1: receipt } };

describe('authoritative node recovery receipt', () => {
  it('retains exact response, generation and receipt', () => {
    expect(nodeRecoveryFromEvent(frame)).toEqual({ responseMessageId: response, executionGeneration: '1', receipt });
  });
  it.each([
    { ...receipt, schema: 'unknown.v1' }, { ...receipt, activation_id: '0'.repeat(64) },
    { ...receipt, journal_revision: 0 }, { ...receipt, journal_revision: Number.MAX_SAFE_INTEGER + 1 },
    { ...receipt, attempt: 17 }, { ...receipt, node_id: 'python\n' }, { ...receipt, node_id: 'x'.repeat(129) },
    { ...receipt, graph_thread: 'x'.repeat(513) }, { ...receipt, graph_thread: 'bad\nthread' },
    { ...receipt, allowed_actions: ['retry', 'reconcile'] }, { ...receipt, allowed_actions: ['reconcile'] },
    { ...receipt, failure_class: 'invalid_input' }, { ...receipt, extra: 'unsafe' },
    { ...receipt, replay_safety: { kind: 'no_external_effect', extra: true } },
  ])('rejects malformed or unknown contracts', (raw) => {
    expect(nodeRecoveryBinding(response, '1', raw)).toBeUndefined();
  });
  it('accepts an unclassified failure only for effect reconciliation', () => {
    const reconcile = { ...receipt, failure_class: 'unknown', stop_reason: 'effect_reconciliation_required',
      replay_safety: { kind: 'unknown_external_effect', effect_id: 'b'.repeat(64) }, allowed_actions: ['reconcile'] };
    expect(nodeRecoveryBinding(response, '1', reconcile)?.receipt.failure_class).toBe('unknown');
    expect(nodeRecoveryBinding(response, '1', { ...receipt, failure_class: 'unknown' })).toBeUndefined();
    expect(nodeRecoveryBinding(response, '1', { ...reconcile, replay_safety: { kind: 'completed_external_effect', receipt_id: 'c'.repeat(64) },
      allowed_actions: ['resume_result'] })).toBeUndefined();
  });
  it.each(['', 'a'.repeat(513), '1\n'])('rejects invalid generation %s', (generation) => {
    expect(nodeRecoveryBinding(response, generation, receipt)).toBeUndefined();
  });
  it.each([
    { parent_agent_name: 'Child' }, { parent_agent_call_id: 'call' }, { parent_agent_path: [{}] },
    { parent_agent_path: 'malformed' }, { metadata: 'malformed' }, { tool_meta: [] }, { metadata: { parent_agent_path: [{}] } },
    { tool_meta: { metadata: { parent_agent_name: 'Child' } } }, { thread_id: 'another-thread' },
  ])('rejects child, malformed ownership or a conflicting thread', (extra) => {
    expect(nodeRecoveryFromEvent({ ...frame, response_metadata: { ...frame.response_metadata, ...extra } })).toBeUndefined();
  });
  it('does not treat another event as a recovery pause', () => {
    expect(nodeRecoveryFromEvent({ ...frame, type: 'unknown_recovery_event' })).toBeUndefined();
  });
});

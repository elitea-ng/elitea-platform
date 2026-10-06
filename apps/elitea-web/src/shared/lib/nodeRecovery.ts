import { z } from 'zod';

const safeInteger = (minimum: number) => z.number().int().min(minimum).refine(Number.isSafeInteger);
const NodeRecoveryID = z.string().length(64).regex(/^[0-9a-f]{64}$/u).refine((value) => value !== '0'.repeat(64));
const NodeRecoveryResponseID = z.string().length(36).regex(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/u)
  .refine((value) => value.replaceAll('-', '') !== '0'.repeat(32));
const NodeRecoveryAction = z.enum(['retry', 'reconcile', 'resume_result']);
const replaySafety = z.discriminatedUnion('kind', [
  z.strictObject({ kind: z.literal('no_external_effect') }),
  z.strictObject({ kind: z.literal('idempotent_effect_not_committed'), effect_id: NodeRecoveryID }),
  z.strictObject({ kind: z.literal('unknown_external_effect'), effect_id: NodeRecoveryID }),
  z.strictObject({ kind: z.literal('completed_external_effect'), receipt_id: NodeRecoveryID }),
  z.strictObject({ kind: z.literal('unclassified') }),
]);
const transientClasses = ['dependency_unavailable', 'rate_limited', 'attempt_timeout', 'worker_interrupted'] as const;
const thread = z.string().min(1).refine((value) => new TextEncoder().encode(value).length <= 512
  && !Array.from(value).some((char) => { const point = char.codePointAt(0)!; return point < 32 || (point >= 127 && point <= 159) || point === 0x2028 || point === 0x2029; }));
/** Mirrors Main domain/noderecovery/contract.go, not worker-local action authority. */
const NodeRecoveryReceipt = z.strictObject({
  schema: z.literal('elitea.pipeline.node-recovery-required.v1'), activation_id: NodeRecoveryID,
  journal_revision: safeInteger(1), node_id: z.string().min(1).max(128).regex(/^[A-Za-z0-9_.:-]+$/u).refine((value) => value.trim() === value), graph_thread: thread,
  step: safeInteger(0), attempt: safeInteger(1).refine((value) => value <= 16),
  failure_class: z.enum([...transientClasses, 'invalid_configuration', 'invalid_input', 'invalid_result', 'model_output_incomplete']),
  stop_reason: z.enum(['operator_approval_required', 'effect_reconciliation_required']),
  replay_safety: replaySafety, allowed_actions: z.tuple([NodeRecoveryAction]),
}).refine((receipt) => {
  const kind = receipt.replay_safety.kind;
  const action = kind === 'no_external_effect' || kind === 'idempotent_effect_not_committed' ? 'retry'
    : kind === 'completed_external_effect' ? 'resume_result' : 'reconcile';
  return receipt.allowed_actions[0] === action && (action === 'retry'
    ? (transientClasses as readonly string[]).includes(receipt.failure_class) && receipt.stop_reason === 'operator_approval_required'
    : receipt.stop_reason === 'effect_reconciliation_required');
});
type NodeRecoveryReceipt = z.infer<typeof NodeRecoveryReceipt>;

export interface NodeRecoveryBinding {
  readonly responseMessageId: string;
  readonly executionGeneration: string;
  readonly receipt: NodeRecoveryReceipt;
}
/** A receipt is pause evidence. It grants no recovery action authority. */
export function nodeRecoveryBinding(responseId: unknown, generation: unknown, raw: unknown): NodeRecoveryBinding | undefined {
  const receipt = NodeRecoveryReceipt.safeParse(raw);
  if (!NodeRecoveryResponseID.safeParse(responseId).success || typeof generation !== 'string'
    || !thread.safeParse(generation).success || !receipt.success) return;
  return { responseMessageId: String(responseId), executionGeneration: generation, receipt: receipt.data };
}
function record(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}
function rootMetadata(source: Record<string, unknown> | undefined): boolean {
  if (!source) return true;
  for (const key of ['parent_agent_name', 'parent_agent_call_id']) {
    if (source[key] !== undefined && source[key] !== '') return false;
  }
  const path = source['parent_agent_path'];
  return path === undefined || (Array.isArray(path) && path.length === 0);
}
export function isRootNodeRecoveryOwner(raw: unknown): boolean {
  if (raw === undefined) return true;
  const meta = record(raw);
  if (!meta) return false;
  const nested = meta['metadata'], tool = meta['tool_meta'];
  if (nested !== undefined && !record(nested)) return false;
  if (tool !== undefined && !record(tool)) return false;
  const toolNested = record(tool)?.['metadata'];
  if (toolNested !== undefined && !record(toolNested)) return false;
  return [meta, record(nested), record(toolNested)].every(rootMetadata);
}
/** Reject child ownership, including malformed hierarchy, before displaying a root pause. */
export function nodeRecoveryFromEvent(raw: unknown): NodeRecoveryBinding | undefined {
  const event = record(raw);
  if (event?.['type'] !== 'agent_node_recovery_required') return;
  const meta = record(event['response_metadata']);
  if (!meta || !isRootNodeRecoveryOwner(meta)) return;
  const binding = nodeRecoveryBinding(event['message_id'], event['execution_generation'], meta['node_recovery_required_v1']);
  if (meta['thread_id'] !== undefined && meta['thread_id'] !== binding?.receipt.graph_thread) return;
  return binding;
}

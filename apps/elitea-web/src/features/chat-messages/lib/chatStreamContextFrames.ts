import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';
import { t } from '@/shared/i18n';
import { normalizeExecutionHierarchy } from './executionHierarchy';
import { replaceAt, nowIso, type ChatStreamContext, type ToolAction } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ChatStreamFrame } from './chatStreamFrame';

/** Context progress belongs to a model scope. It never clears answer text,
 * interrupts or tool results, and parallel children never share its identity. */
export function reduceContextFrame(
  history: readonly ChatMessage[], frame: ChatStreamFrame, type: string,
  context: ChatStreamContext, index: number,
): readonly ChatMessage[] | undefined {
  if (type !== 'agent_context_status') return undefined;
  const current = history[index];
  const metadata = frame.response_metadata;
  const status = metadata?.context_status as { version?: unknown; phase?: unknown } | undefined;
  if (!current || status?.version !== 1 || !['measured', 'compacting', 'compacted'].includes(String(status.phase))) return history;
  const hierarchy = normalizeExecutionHierarchy(metadata);
  const id = `context:${JSON.stringify([frame.execution_generation, hierarchy.parent_agent_path, hierarchy.parent_agent_call_id, metadata?.node_name])}`;
  const actions = (current.toolActions ?? []) as readonly ToolAction[];
  const existing = actions.find((action) => action.id === id);
  // Normal measurement is meter-only. A recovered request can settle a pending
  // compacting notice without claiming a new summarization took place.
  if (status.phase === 'measured' && (!existing || existing.status !== ToolActionStatus.processing)) return history;
  const compacting = status.phase === 'compacting';
  const text = compacting ? t('chatMessages.context.compacting', 'Compacting context…')
    : status.phase === 'compacted' ? t('chatMessages.context.compacted', 'Context compacted')
      : t('chatMessages.context.ready', 'Context ready');
  const action: ToolAction = {
    ...existing, ...hierarchy, id,
    name: t('chatMessages.context.name', 'Context compaction'),
    status: compacting ? ToolActionStatus.processing : ToolActionStatus.complete,
    type: TOOL_ACTION_TYPES.Summary,
    contextProgress: true,
    toolMeta: { ...hierarchy, node_name: metadata?.node_name },
    created_at: existing?.created_at ?? Date.parse(nowIso(context)),
    ended_at: compacting ? undefined : Date.parse(nowIso(context)),
    message: compacting ? text : undefined,
    content: text,
  };
  return replaceAt(history, index, {
    toolActions: existing ? actions.map((old) => old.id === id ? action : old) : [...actions, action],
  });
}

export function isRootContextFrame(frame: ChatStreamFrame): boolean {
  if (frame.type !== 'agent_context_status' || frame.response_metadata?.model_scope !== 'agent') return false;
  const owner = normalizeExecutionHierarchy(frame.response_metadata);
  return !owner.parent_agent_name && !owner.parent_agent_call_id && owner.parent_agent_path.length === 0;
}

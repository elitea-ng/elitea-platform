/**
 * lib/chatStreamThinkingFrames.ts — the thinking-step family.
 *
 * Owns `agent_thinking_step` and `agent_thinking_step_update`, plus the
 * per-step application `agent_llm_end` reuses for its end-of-turn fan-out (the
 * only reason `applyThinkingStep`/`isEmptyTransition` are exported rather than
 * private here). Split out of `chatStreamReducer.ts` for the §3.5 file-length
 * budget — that is the honest reason, and the only one; the code is the
 * original switch arms and their comments, moved unchanged.
 */
import { appendToolOutputChunk } from './toolOutputChunks';
import { convertJsonToString } from '@/shared/lib/json';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import { normalizeExecutionHierarchy } from './executionHierarchy';
import {
  findToolAction,
  replaceAt,
  replaceToolAction,
  toolMetadata,
  type ToolAction,
} from './chatStreamShared';

import type { ChatMessage } from './convertMessagesToChatHistory';
import { SocketMessageType, type ChatStreamFrame, type ThinkingStep } from './chatStreamFrame';

/**
 * Apply one `thinking_steps` entry to the action it describes.
 *
 * Returns `undefined` when the step names no action we know, so the caller can
 * tell "nothing to update" from "updated to nothing".
 */
export function applyThinkingStep(action: ToolAction, step: ThinkingStep): ToolAction {
  let assembled = step;
  let outputState = { ...action.toolMeta };
  for (const field of ['text', 'thinking'] as const) {
    const key = `${field}_chunk_v1`;
    if (!step[key]) continue;
    const prior = outputState[key];
    const merged = appendToolOutputChunk(prior ? outputState[`${field}_assembled`] : '', step[field], step[key], prior, 4194304);
    if (!merged) return action;
    outputState = { ...outputState, [key]: merged.chunk, [`${field}_assembled`]: merged.output };
    assembled = { ...assembled, [field]: merged.output };
  }
  if (step['text_chunk_v1'] || step['thinking_chunk_v1']) {
    assembled = { ...assembled, text: outputState['text_assembled'] ?? action.content ?? '', thinking: outputState['thinking_assembled'] as string | undefined };
  }
  step = assembled;
  const hierarchy = normalizeExecutionHierarchy(step, step.metadata, step.message?.response_metadata?.metadata, action, action.toolMeta);
  // The backend normalises `text` for every provider. The "with inputs {...}"
  // tail is a verbose restatement of arguments the UI already shows, and the
  // baseline strips it rather than rendering it twice.
  const text = convertJsonToString(step.text ?? '', true).replace(/\s+with inputs\s+\{[^}]*\}/g, '');
  const stepModelName = step.message?.response_metadata?.model_name;
  const correctToolName = step.message?.response_metadata?.tool_name;
  const existingMeta = outputState;
  const modelName = existingMeta['ls_model_name'];

  const toolMeta: Record<string, unknown> = { ...existingMeta, ...hierarchy };
  if (stepModelName && !modelName) toolMeta['ls_model_name'] = stepModelName;

  const updated: Record<string, unknown> = {
    ...action,
    ...hierarchy,
    content: text,
    ended_at: step.timestamp_finish,
    toolMeta,
  };
  // Only a real node name, never the model's own name echoed back.
  if (correctToolName?.trim() && correctToolName !== toolMeta['ls_model_name']) updated['name'] = correctToolName;
  if (step.thinking) updated['thinking'] = step.thinking;
  return updated as unknown as ToolAction;
}

/**
 * A step that produced no text and belongs to no parent agent is a graph
 * transition, not something a user asked for; the baseline drops it so the
 * timeline shows work rather than plumbing.
 */
export function isEmptyTransition(updated: ToolAction): boolean {
  const content = updated['content'];
  const hasText = typeof content === 'string' && content.trim().length > 0;
  return !hasText && !updated.parent_agent_name;
}

/**
 * Reduce one thinking-step frame, or return `undefined` for a frame this family
 * does not own so the dispatcher can offer it to the next one.
 */
export function reduceThinkingFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  index: number,
): readonly ChatMessage[] | undefined {
  switch (type) {
    // A step reports progress for a tool that may not have started yet: a
    // thinking step can arrive BEFORE its agent_tool_start, so this creates a
    // placeholder rather than dropping the progress.
    case SocketMessageType.AgentThinkingStep: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current) return history;
      const metadata = toolMetadata(frame);
      const actions = (current.toolActions ?? []) as readonly ToolAction[];
      const existing = runId ? actions.find((action) => action.id === runId) : undefined;
      const hierarchy = normalizeExecutionHierarchy(
        frame.response_metadata?.metadata,
        frame.response_metadata?.tool_meta?.metadata,
        existing,
        existing?.toolMeta,
      );

      if (existing && runId) {
        return replaceAt(history, index, {
          toolActions: replaceToolAction(current, runId, (action) => ({
            ...action,
            ...hierarchy,
            message: frame.response_metadata?.message,
            // `?? true`, not the baseline's `|| true`, which can only ever
            // yield true and so silently ignored an explicit `markdown: false`.
            markdown: frame.response_metadata?.markdown ?? true,
            toolMeta: { ...action.toolMeta, ...metadata, ...hierarchy },
          })),
        });
      }

      // The baseline's id here is `'thinking_step_' + toolRunId || '' + v4()`,
      // which parses as `('thinking_step_' + toolRunId) || ('' + v4())` — always
      // truthy, so the uuid fallback never ran and every id-less step collided
      // on the literal "thinking_step_undefined". The intent is a unique id.
      const placeholderId = `thinking_step_${runId ?? crypto.randomUUID()}`;
      const draft: Record<string, unknown> = {
        id: placeholderId,
        name: TOOL_ACTION_TYPES.Toolkit,
        type: TOOL_ACTION_TYPES.Toolkit,
        status: ToolActionStatus.processing,
        ...hierarchy,
        toolInputs: frame.response_metadata?.tool_inputs,
        toolOutputs: frame.response_metadata?.tool_outputs,
        toolMeta: { ...metadata, ...hierarchy },
        responseMetadata: frame.response_metadata,
        created_at: frame.created_at,
        markdown: frame.response_metadata?.markdown ?? true,
        renderHtml: frame.response_metadata?.render_html ?? false,
        message: frame.response_metadata?.message,
        content: '',
      };
      return replaceAt(history, index, {
        toolActions: [...actions, draft as unknown as ToolAction],
      });
    }

    // Progress text for a step already on the timeline.
    case SocketMessageType.AgentThinkingStepUpdate: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current || !runId || !findToolAction(current, runId)) return history;
      const toolMetaFromFrame = frame.response_metadata?.tool_meta;

      return replaceAt(history, index, {
        toolActions: replaceToolAction(current, runId, (action) => {
          const hierarchy = normalizeExecutionHierarchy(
            frame.response_metadata?.metadata,
            frame.response_metadata?.tool_meta?.metadata,
            action,
            action.toolMeta,
          );
          return {
            ...action,
            ...hierarchy,
            message: convertJsonToString(frame.response_metadata?.message, true),
            markdown: frame.response_metadata?.markdown ?? false,
            // Only when the frame supplies one: the toolkit badge reads this,
            // and overwriting it with nothing would blank the badge mid-run.
            ...(toolMetaFromFrame ? { toolMeta: { ...action.toolMeta, ...toolMetaFromFrame, ...hierarchy } } : {}),
          } as ToolAction;
        }),
      });
    }

    default:
      return undefined;
  }
}

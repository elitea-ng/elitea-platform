/**
 * lib/chatStreamToolFrames.ts — the tool-lifecycle family.
 *
 * Owns `agent_tool_start`/`agent_tool_end`/`agent_tool_error` and the
 * `mcp_authorization_required` card, together with the toolkit-identity and
 * MCP-token-key helpers only these frames need, and `mcpSessionFromFrame` —
 * the one piece of this family a CALLER consumes (it is re-exported from
 * `chatStreamReducer.ts`, so no importer moved). Separate file purely for the
 * §3.5 file-length budget; the switch arms and every comment on them are the
 * originals, moved unchanged.
 */
import { appendToolOutputChunk } from './toolOutputChunks';

import { convertJsonToString } from '@/shared/lib/json';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';
import { buildAuthorizationActions } from '@/entities/message';

import { normalizeExecutionHierarchy } from './executionHierarchy';
import {
  findToolAction,
  replaceAt,
  replaceToolAction,
  threadIdOf,
  toolMetadata,
  type ToolAction,
} from './chatStreamShared';

import type { ChatMessage } from './convertMessagesToChatHistory';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';

/**
 * Recover a toolkit name the metadata omitted from the tool's description.
 * Two shapes are in the wild — `[Toolkit: name]` (vectorstore, inventory) and a
 * `Toolkit: name` line (most others) — and the baseline tries both before
 * giving up, which is why a single regex here would silently unlabel toolkits.
 */
function toolkitNameFromDescription(description: unknown): string {
  if (typeof description !== 'string') return '';
  const bracketed = /\[Toolkit:\s*([^\]]+)]/.exec(description);
  if (bracketed?.[1]) return bracketed[1].trim();
  const line = /(?:^|\n)Toolkit:\s*([^\n]+)/.exec(description);
  return line?.[1]?.trim() ?? '';
}

/** Toolkit identity, in the baseline's precedence order. */
function toolkitIdentity(frame: ChatStreamFrame, metadata: Record<string, unknown>): {
  readonly name: string;
  readonly type: string;
} {
  const responseMetadata = frame.response_metadata;
  const rawToolName = responseMetadata?.tool_name ?? '';
  // The pre-rename wire format encoded the toolkit in the tool name itself.
  const legacyToolkit = rawToolName.includes('___') ? (rawToolName.split('___')[0] ?? '') : '';
  const fromMetadata = typeof metadata['toolkit_name'] === 'string' ? (metadata['toolkit_name'] as string) : '';
  const fromDescription = toolkitNameFromDescription(responseMetadata?.tool_meta?.description);
  const typeFromMetadata = typeof metadata['toolkit_type'] === 'string' ? (metadata['toolkit_type'] as string) : '';
  return {
    name: fromMetadata || fromDescription || responseMetadata?.toolkit_name || legacyToolkit,
    type: typeFromMetadata || responseMetadata?.toolkit_type || '',
  };
}

/**
 * The tool's display name. A lazy-loading wrapper puts its own class name in
 * `tool_name` and signals the swap with `metadata.original_name`; the real name
 * is then on `tool_meta.name`, and preferring it is what stops the UI showing
 * "LazyLoading" instead of the tool the user invoked.
 */
function toolDisplayName(frame: ChatStreamFrame, metadata: Record<string, unknown>): string | undefined {
  const responseMetadata = frame.response_metadata;
  const wrapped = metadata['original_name'] && responseMetadata?.tool_meta?.name;
  return wrapped ? (responseMetadata.tool_meta?.name as string) : responseMetadata?.tool_name;
}

/**
 * The MCP session a tool frame reports, for a CALLER to persist.
 *
 * The baseline calls `McpAuthHelpers.setSessionId` from inside the reducer.
 * That is I/O, so it cannot live here — but dropping it would silently break
 * MCP re-authorization, so the pair is surfaced instead of discarded.
 */
export function mcpSessionFromFrame(
  frame: ChatStreamFrame,
): { readonly serverUrl: string; readonly sessionId: string } | undefined {
  const metadata = toolMetadata(frame);
  const sessionId = metadata['mcp_session_id'];
  const serverUrl = metadata['mcp_server_url'];
  if (typeof sessionId !== 'string' || typeof serverUrl !== 'string' || !sessionId || !serverUrl) return undefined;
  return { serverUrl, sessionId };
}

/**
 * Reduce one tool-lifecycle frame, or return `undefined` for a frame this
 * family does not own so the dispatcher can offer it to the next one.
 */
export function reduceToolFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  index: number,
): readonly ChatMessage[] | undefined {
  switch (type) {
    // A tool started. Creates the timeline entry the UI renders as a chip; a
    // repeat for the same run id updates ancestry rather than duplicating it.
    case SocketMessageType.AgentToolStart: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current || !runId) return history;
      const metadata = toolMetadata(frame);
      const hierarchy = normalizeExecutionHierarchy(metadata, frame.response_metadata);
      const threadId = threadIdOf(frame);
      const existing = findToolAction(current, runId);

      if (existing) {
        return replaceAt(history, index, {
          toolActions: replaceToolAction(current, runId, (action) => ({
            ...action,
            ...hierarchy,
            toolMeta: { ...action.toolMeta, ...metadata, ...hierarchy },
          })),
          ...(threadId !== undefined ? { threadId } : {}),
        });
      }

      const toolkit = toolkitIdentity(frame, metadata);
      // Built imperatively, not with conditional spreads: spreading a
      // `{name: X} | {}` union infers `name?: X | undefined`, which
      // `exactOptionalPropertyTypes` rejects against `SubAgentGroupable`'s
      // "absent or string". Same convention as
      // `useToolkitChatSocket.hooks.ts:79-90`.
      const draft: Record<string, unknown> = {
        id: runId,
        type: TOOL_ACTION_TYPES.Tool,
        status: ToolActionStatus.processing,
        message: '',
        ...hierarchy,
        toolInputs: frame.response_metadata?.tool_inputs,
        toolOutputs: frame.response_metadata?.tool_outputs,
        toolMeta: { ...metadata, toolkit_type: toolkit.type, toolkit_name: toolkit.name, ...hierarchy },
        created_at: frame.response_metadata?.timestamp_start ?? frame.created_at,
      };
      const displayName = toolDisplayName(frame, metadata);
      if (displayName !== undefined) draft['name'] = displayName;
      if (typeof metadata['original_name'] === 'string') draft['original_name'] = metadata['original_name'];
      const created = draft as unknown as ToolAction;
      return replaceAt(history, index, {
        toolActions: [...((current.toolActions ?? []) as readonly ToolAction[]), created],
        ...(threadId !== undefined ? { threadId } : {}),
      });
    }

    // The tool returned. Outputs ACCUMULATE — a string appends, an object
    // merges — because a tool may report progressively, and replacing would
    // discard everything but the last frame.
    case SocketMessageType.AgentToolEnd:
    case SocketMessageType.AgentToolError: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current || !runId || !findToolAction(current, runId)) return history;
      const metadata = toolMetadata(frame);
      const isError = type === SocketMessageType.AgentToolError;
      if (isError && frame.response_metadata?.tool_output_chunk_v1 === undefined) {
        return replaceAt(history, index, {
          toolActions: replaceToolAction(current, runId, (action) => {
            const hierarchy = normalizeExecutionHierarchy(metadata, action, action.toolMeta);
            return {
              ...action, ...hierarchy,
              content: convertJsonToString(frame.content ?? ''),
              status: ToolActionStatus.error, ended_at: frame.created_at, isError: true,
              toolMeta: { ...action.toolMeta, ...metadata, ...hierarchy },
            };
          }),
        });
      }

      return replaceAt(history, index, {
        toolActions: replaceToolAction(current, runId, (action) => {
          const hierarchy = normalizeExecutionHierarchy(metadata, action, action.toolMeta);
          const output = frame.response_metadata?.tool_output;
          const previous = action['toolOutputs'];
          let toolOutputs = previous;
          const chunk = frame.response_metadata?.tool_output_chunk_v1;
          const assembled = chunk === undefined ? undefined : appendToolOutputChunk(previous, output, chunk, action['toolOutputChunk']);
          if (chunk !== undefined && assembled === undefined) return action;
          if (assembled && assembled.output === previous && assembled.chunk === action['toolOutputChunk']) return action;
          if (assembled) {
            toolOutputs = assembled.output;
          } else if (typeof output === 'string') {
            toolOutputs = (typeof previous === 'string' ? previous : '') + convertJsonToString(output, true);
          } else if (typeof output === 'object' && output !== null) {
            toolOutputs = { ...((typeof previous === 'object' && previous !== null ? previous : {}) as object), ...output };
          }
          return {
            ...action,
            ...hierarchy,
            toolOutputs,
            ...(assembled ? { toolOutputChunk: assembled.chunk } : {}),
            message: undefined,
            content: convertJsonToString(frame.content ?? ''),
            // An action awaiting approval stays awaiting it: the wrapper ending
            // is not the user answering.
            status: action.status === ToolActionStatus.actionRequired || (assembled && !assembled.chunk.final) ? action.status : isError ? ToolActionStatus.error : ToolActionStatus.complete,
            ...(isError ? { isError: true } : {}),
            ended_at: assembled && !assembled.chunk.final ? action['ended_at'] : frame.response_metadata?.timestamp_finish ?? frame.created_at,
            created_at: frame.response_metadata?.timestamp_start ?? action['created_at'],
            toolMeta: { ...action.toolMeta, ...metadata, ...hierarchy },
          };
        }),
      });
    }

    // An MCP toolkit answered 401. This is a tool action, not a message: the
    // authorization card renders as a timeline entry so the rest of the turn's
    // work stays visible behind it.
    case SocketMessageType.McpAuthorizationRequired: {
      if (index === -1) return history;
      const current = history[index];
      if (!current) return history;
      const drafts = buildAuthorizationActions(
        frame.response_metadata ?? {},
        convertJsonToString(frame.content ?? 'Authorization required.', true),
        String(frame.created_at ?? ''),
      );
      let next = [...((current.toolActions ?? []) as readonly ToolAction[])];
      for (const draft of drafts) {
        const hierarchy = normalizeExecutionHierarchy(draft.toolMeta?.['metadata'], draft.toolMeta, draft);
        const enriched = { ...draft, ...hierarchy, toolMeta: { ...draft.toolMeta, ...hierarchy } } as ToolAction;
        const existing = next.findIndex((action) => action.id === draft.id);
        if (existing === -1) next.push(enriched);
        else next[existing] = enriched;
      }

      // The user has to act, so nothing is in flight any more.
      return replaceAt(history, index, {
        toolActions: next,
        isLoading: false,
        isStreaming: false,
        isRegenerating: false,
      });
    }

    default:
      return undefined;
  }
}

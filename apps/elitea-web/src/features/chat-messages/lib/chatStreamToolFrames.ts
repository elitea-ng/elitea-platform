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
import { convertJsonToString } from '@/shared/lib/json';
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import { normalizeExecutionHierarchy } from './executionHierarchy';
import {
  TOOL_OUTPUT_CHUNKS_KEY,
  chunkedCompletionOf,
  finishChunkedToolOutput,
} from './chatStreamToolOutputChunks';
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
 * The key the backend will look this toolkit's OAuth token up under.
 *
 * This is NOT cosmetic and NOT always the server URL: the backend resolves
 * tokens from `kwargs['tokens']` by a key that differs per toolkit family, so
 * getting it wrong stores a token the toolkit will never find and the user is
 * asked to authorize again on every call.
 *
 *   pre-built MCP (`mcp_*`)   the toolkit type — the backend matches by
 *                             server/toolkit name, not by OAuth server URL
 *   delegated OAuth with a    `{configuration_uuid}:{oauth endpoint}`, the
 *   configuration uuid        SDK's primary composite lookup
 *   SharePoint / OpenAPI      the discovery endpoint
 *   regular MCP               the MCP server URL, which IS its OAuth endpoint
 */
function mcpTokenStorageKey(frame: ChatStreamFrame, serverUrl: string): string {
  const responseMetadata = frame.response_metadata;
  const resource = responseMetadata?.resource_metadata;
  const authServers = resource?.authorization_servers ?? responseMetadata?.authorization_servers;
  const oauthEndpoint = authServers?.[0];
  const configUuid = resource?.configuration_uuid;
  const toolkitType = responseMetadata?.toolkit_type;

  if (typeof toolkitType === 'string' && toolkitType.startsWith('mcp_') && toolkitType !== 'mcp') return toolkitType;
  if (configUuid && oauthEndpoint) return `${configUuid}:${oauthEndpoint}`;
  if (resource?.resource_name === 'SharePoint' || resource?.resource_name === 'OpenAPI') {
    return oauthEndpoint ?? serverUrl;
  }
  return serverUrl;
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
    case SocketMessageType.AgentToolEnd: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current || !runId || !findToolAction(current, runId)) return history;
      const metadata = toolMetadata(frame);

      return replaceAt(history, index, {
        toolActions: replaceToolAction(current, runId, (action) => {
          const hierarchy = normalizeExecutionHierarchy(metadata, action, action.toolMeta);
          const output = frame.response_metadata?.tool_output;
          const previous = action['toolOutputs'];
          let toolOutputs = previous;
          // A CHUNKED result (#956) is finished from the chunks that preceded
          // this frame, never from its `tool_output` — which is the empty
          // string by construction, and would otherwise append nothing onto a
          // pin that never received the text at all.
          const chunked = chunkedCompletionOf(frame);
          let chunkState: unknown;
          let partial = false;
          if (chunked) {
            const finished = finishChunkedToolOutput(action, chunked);
            toolOutputs = finished.toolOutputs;
            chunkState = finished.state;
            partial = finished.state.partial === true;
          } else if (typeof output === 'string') {
            toolOutputs = (typeof previous === 'string' ? previous : '') + convertJsonToString(output, true);
          } else if (typeof output === 'object' && output !== null) {
            toolOutputs = { ...((typeof previous === 'object' && previous !== null ? previous : {}) as object), ...output };
          }
          return {
            ...action,
            ...hierarchy,
            toolOutputs,
            ...(chunked ? { [TOOL_OUTPUT_CHUNKS_KEY]: chunkState, toolOutputPartial: partial } : {}),
            message: undefined,
            content: convertJsonToString(frame.content ?? ''),
            // An action awaiting approval stays awaiting it: the wrapper ending
            // is not the user answering.
            status: action.status === ToolActionStatus.actionRequired ? action.status : ToolActionStatus.complete,
            ended_at: frame.response_metadata?.timestamp_finish ?? frame.created_at,
            created_at: frame.response_metadata?.timestamp_start ?? action['created_at'],
            toolMeta: { ...action.toolMeta, ...metadata, ...hierarchy },
          };
        }),
      });
    }

    case SocketMessageType.AgentToolError: {
      if (index === -1) return history;
      const current = history[index];
      const runId = frame.response_metadata?.tool_run_id;
      if (!current || !runId || !findToolAction(current, runId)) return history;
      const metadata = toolMetadata(frame);

      return replaceAt(history, index, {
        toolActions: replaceToolAction(current, runId, (action) => {
          const hierarchy = normalizeExecutionHierarchy(metadata, action, action.toolMeta);
          return {
            ...action,
            ...hierarchy,
            content: convertJsonToString(frame.content ?? ''),
            status: ToolActionStatus.error,
            ended_at: frame.created_at,
            isError: true,
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
      const responseMetadata = frame.response_metadata;
      const runId = responseMetadata?.tool_run_id ?? crypto.randomUUID();
      const toolName = responseMetadata?.tool_name ?? 'MCP toolkit';
      const serverUrl = responseMetadata?.server_url ?? 'MCP server';
      const statusCode = responseMetadata?.status ?? 401;
      const authServers = responseMetadata?.resource_metadata?.authorization_servers ?? responseMetadata?.authorization_servers;
      const hasAuthServers = Boolean(authServers && authServers.length > 0);

      const draft: Record<string, unknown> = {
        name: toolName,
        id: runId,
        status: hasAuthServers ? ToolActionStatus.actionRequired : ToolActionStatus.error,
        toolInputs: undefined,
        toolOutputs: hasAuthServers
          ? {
              resource_metadata_url: responseMetadata?.resource_metadata_url ?? null,
              authorization_servers: authServers,
              server_url: mcpTokenStorageKey(frame, serverUrl),
            }
          : undefined,
        toolMeta: responseMetadata,
        created_at: frame.created_at,
        ended_at: frame.created_at,
        type: TOOL_ACTION_TYPES.Toolkit,
        markdown: false,
        renderHtml: false,
        content: hasAuthServers
          ? // `authServers` is non-empty on this branch by construction.
            `${convertJsonToString(frame.content ?? 'Authorization required.', true)}${
              responseMetadata?.resource_metadata_url
                ? `\n\nResource metadata: ${responseMetadata.resource_metadata_url}`
                : `\n\nAuthorization servers: ${(authServers ?? []).join(', ')}`
            }`
          : `${statusCode}: Authorization error in "${toolName}" toolkit.\n\n` +
            `The MCP server at ${serverUrl} requires OAuth authorization, but the server ` +
            `did not provide the authorization server configuration. ` +
            `Please contact the server administrator or check the toolkit configuration.`,
      };

      const actions = (current.toolActions ?? []) as readonly ToolAction[];
      const exists = actions.some((action) => action.id === runId);
      const next = exists
        ? actions.map((action) => (action.id === runId ? ({ ...action, ...draft } as ToolAction) : action))
        : [...actions, draft as unknown as ToolAction];

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

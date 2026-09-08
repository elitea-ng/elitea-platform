/**
 * hooks/useChatBoxHandlers.mcpAuth.ts — the MCP-authorization continuation body.
 *
 * `resumeMcpFlow` resumes an MCP-authorization pause over REST
 * (`POST /elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}`
 * with `execution_contract=agent.continue.authorization.v1`), the same way
 * `continueHitl`/`continueTokenLimit` already resume theirs. The route
 * validates every field of that body, so the shaping lives here beside the
 * rules it obeys, exactly as `./useChatBoxHandlers.hitl.ts` does for HITL.
 *
 * WHY THIS FILE EXISTS AT ALL. Until now this one pause was socket-only, for a
 * stated reason: the contract requires an `authorization_request_id` and
 * "nothing in this app captures that field off the `mcp_authorization_required`
 * frame". It does now — `authorizationRequestId` below reads it back off the
 * tool action the reducer built from that frame. The socket client is a no-op
 * stub whenever `vite_socket_server` is empty, which is what the shipped
 * deployment serves, so the socket alone left every MCP approval paused
 * server-side with no way back on screen.
 *
 * THE IDENTITY IS A COALESCE, NOT A FIELD. `mcp_authorization_required`
 * metadata carries `interrupt_id`, and may instead carry `tool_run_id` or
 * `tool_call_id`. The server stores the request as-is and reads its identity
 * back with `COALESCE(interrupt_id, tool_run_id, tool_call_id)`
 * (`ResolveCurrentAuthorizationContinuation`, services/elitea-main/internal/db/
 * queries/agent_chat.sql; the worker picks the same three, in the same order,
 * in `authorization_identity`). Reading one fixed key here would address a
 * request the server files under a different one, and the resume would be
 * refused 422 for a pause that is perfectly resumable.
 *
 * DISCLOSED GAP, unchanged by this port: `mcp_tokens` travels as `{}`. The
 * route REQUIRES the field to be a JSON object, and this app has never
 * collected the browser-held OAuth tokens for any run payload — the same gap
 * `features/toolkits/ui/IndexesTab.tsx` records for the index-run payload, and
 * the socket path this replaces sent no tokens either. So an `authorize` that
 * depends on a browser-held token is no better off than before, and every
 * other case is strictly better off: it now reaches a transport at all.
 */
import type { ChatMessage } from '@/features/chat-messages';
import { conversationApi } from '@/entities/conversation';
import { ToolActionStatus } from '@/shared/lib/chat';

import {
  buildChatContinuePayload,
  buildDeclinedServersList,
  findActionRequiredToolAction,
  findQuestionText,
  readServerUrl,
  revertContinuation,
  trackMcpAuthDecision,
  tryEmit,
} from './useChatBoxHandlers.helpers';
import type { ChatBoxHandlerDeps, ToolActionLike } from './useChatBoxHandlers.helpers';
import { undeliveredText } from './useChatBoxHandlers.turns';

/** The two `authorization_action` values the Go route admits (`currentAuthorizationAction`). */
type McpAuthorizationAction = 'authorize' | 'skip';

/** The metadata keys that can carry the pause's identity, in the server's own COALESCE order. */
const IDENTITY_KEYS: readonly string[] = ['interrupt_id', 'tool_run_id', 'tool_call_id'];

/**
 * The `authorization_request_id` of the pause this tool action represents, or
 * `undefined` when the frame carried no identity at all.
 *
 * `undefined` means "no REST body fits" and the caller then emits over the
 * socket, the same contract `buildHitlContinueBody` already has.
 */
function authorizationRequestId(action: ToolActionLike | undefined): string | undefined {
  const meta = action?.toolMeta ?? {};
  for (const key of IDENTITY_KEYS) {
    const value = meta[key];
    if (typeof value === 'string' && value !== '') return value;
  }
  return undefined;
}

interface McpAuthorizationContinueBodyParams {
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly messageId: string;
  readonly threadId?: string | undefined;
  readonly authorizationRequestId: string | undefined;
  /** `true` when the user pressed "Skip Auth"; the run continues without the toolkit. */
  readonly declined: boolean;
  /** Session-scoped declined-server bookkeeping — the route admits a NON-EMPTY array here, unlike the HITL contract. */
  readonly declinedServers: readonly Record<string, unknown>[];
}

/**
 * The MCP-authorization continuation body, or `undefined` when this pause
 * cannot be expressed in the contract.
 *
 * Every rule below is the route's, not a preference
 * (`services/elitea-main/internal/api/v2/agentexecution/route.go`'s
 * `CurrentAuthorizationContinuationContract` arm):
 *
 *  - `project_id` is a NUMBER equal to the path project; the socket payload
 *    sends a string, so it is rebuilt rather than reused.
 *  - `conversation_uuid` equals the path conversation and `message_id` is not
 *    empty.
 *  - `mcp_tokens` must BE a JSON object and `ignored_mcp_servers` /
 *    `user_declined_mcp_servers` must BE JSON arrays — present, not merely
 *    absent-and-tolerated, which is the opposite of the HITL arm's rule that
 *    all three be absent or empty.
 *  - `hitl_resume`, `hitl_action`, `hitl_value` and `hitl_decisions` are all
 *    refused beside a single-request authorization, so none is sent.
 */
function buildMcpAuthorizationContinueBody(
  params: McpAuthorizationContinueBodyParams,
): Record<string, unknown> | undefined {
  const projectId = Number(params.projectId);
  if (!Number.isSafeInteger(projectId) || projectId <= 0) return undefined;
  if (!params.conversationUuid || !params.messageId || !params.authorizationRequestId) return undefined;
  const action: McpAuthorizationAction = params.declined ? 'skip' : 'authorize';
  return {
    project_id: projectId,
    conversation_uuid: params.conversationUuid,
    message_id: params.messageId,
    ...(params.threadId ? { thread_id: params.threadId } : {}),
    authorization_request_id: params.authorizationRequestId,
    authorization_action: action,
    mcp_tokens: {},
    ignored_mcp_servers: [],
    user_declined_mcp_servers: params.declinedServers,
  };
}

/**
 * The `resumeMcpFlow` handler: REST first, socket second.
 *
 * A factory over `deps` rather than a closure inside `useChatBoxHandlers`, for
 * the same reason `createSendQuestion`/`createRegenerateAnswer` are — the hook
 * file is at its §3.5 length budget, and this handler's rules belong beside
 * the contract they obey anyway.
 *
 * The optimistic patch below is irreversible unless BOTH transports are
 * checked: the authorization card is already gone and the bubble already
 * spinning, so a resume that reached nothing would leave the run paused
 * server-side with no way back on screen.
 */
export function createResumeMcpFlow(
  deps: ChatBoxHandlerDeps,
): (messageId: string, addToIgnoreList?: boolean) => Promise<void> {
  const resumeOverRest = async (
    message: ChatMessage,
    authRequiredAction: ToolActionLike | undefined,
    declined: boolean,
  ): Promise<boolean> => {
    if (!deps.continueStreamedExecution || !deps.conversationUuid) return false;
    const body = buildMcpAuthorizationContinueBody({
      projectId: deps.projectId,
      conversationUuid: deps.conversationUuid,
      messageId: message.id,
      threadId: message.threadId,
      authorizationRequestId: authorizationRequestId(authRequiredAction),
      declined,
      declinedServers: buildDeclinedServersList(deps.sessionDeclinedMcpServersRef),
    });
    if (body === undefined) return false;
    const outcome = await deps.continueStreamedExecution({
      conversationUuid: deps.conversationUuid,
      contract: conversationApi.contracts.continueAuthorization,
      body,
    });
    return outcome.started;
  };

  return async (messageId: string, addToIgnoreList = false): Promise<void> => {
    const message = deps.chatHistory.find((item) => item.id === messageId);
    if (!message) return;
    const authRequiredAction = findActionRequiredToolAction(message);
    trackMcpAuthDecision(
      deps.sessionDeclinedMcpServersRef,
      authRequiredAction,
      readServerUrl(authRequiredAction),
      addToIgnoreList,
    );
    const question = findQuestionText(deps.chatHistory, message) ?? 'Continue';
    const payload: Record<string, unknown> = {
      ...buildChatContinuePayload(deps, { messageId, threadId: message.threadId, question }),
      user_declined_mcp_servers: buildDeclinedServersList(deps.sessionDeclinedMcpServersRef),
    };
    deps.setChatHistory((prev) =>
      prev.map((msg) =>
        msg.id !== messageId
          ? msg
          : {
              ...msg,
              isLoading: true,
              isStreaming: true,
              toolActions: ((msg.toolActions ?? []) as readonly ToolActionLike[]).filter(
                (action) => action.status !== ToolActionStatus.actionRequired,
              ) as unknown as ChatMessage['toolActions'],
            },
      ),
    );
    deps.setStreamingInfo(message.questionId ?? messageId);
    // REST first, socket second — the same order every other continuation
    // takes, and for the same reason: the socket client is a no-op stub
    // whenever `vite_socket_server` is empty.
    if (await resumeOverRest(message, authRequiredAction, addToIgnoreList)) return;
    if (!tryEmit(() => deps.emitSocket('chat_continue_predict', payload), 'resumeMcpFlow')) {
      revertContinuation(deps.setChatHistory, message, undeliveredText());
    }
  };
}

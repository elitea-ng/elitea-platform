import { useEditorTestTransport } from './useEditorTestTransport';
import type { ChatBoxProps } from '../ChatBox.types';
/** Binds ChatBox send, continuation, and regeneration to the REST/SSE transport. */
import { useCallback, useMemo } from "react";

import { useChatStreamTransport, type ChatMessage } from "@/features/chat-messages";
import { conversationApi, contextManagementApi } from "@/entities/conversation";
import { getExecutionTokens } from "@/features/mcps";
import type { useUploadAttachments } from "@/entities/conversation";

// Derive these types through the public entity API; avoid a cross-slice deep import.
type UploadAttachments = ReturnType<typeof useUploadAttachments>["uploadAttachments"];
type UploadAttachmentsParams = Parameters<UploadAttachments>[0];
type UploadAttachmentsOutcome = Awaited<ReturnType<UploadAttachments>>;
import { getConfig } from "@/shared/config";
import type { ExecutionEventData } from "@/shared/api/sse";
import { t } from "@/shared/i18n";

import { pickIdAndUuid } from "../ChatBox.helpers";
import {
  NO_STREAM_TRANSPORT,
  STREAM_STARTED,
  type StreamStartOutcome,
} from "./useChatBoxHandlers.helpers";
import {
  adhocParticipants,
  buildChatStreamContext,
  buildRegenerateBody,
  buildStartBody,
  creationMeta,
  internalToolsSaveFailure,
  positiveParticipantId,
  resolveSendModelName,
  resolveStartContract,
  resolveTargetParticipant,
} from "./useChatBoxSend.helpers";

/** The conversation-lifecycle and attachment-upload slices this hook adapts. */
interface SendDeps {
  readonly createConversation: (input: {
    name: string;
    isPrivate: boolean;
    participants?: readonly unknown[];
    meta?: Readonly<Record<string, unknown>>;
  }) => Promise<
    { readonly id?: string | number; readonly uuid?: string } | undefined
  >;
  readonly uploadAttachments: (
    input: UploadAttachmentsParams,
  ) => Promise<UploadAttachmentsOutcome>;
}

/** @public Params for `useChatBoxSend`. */
export interface UseChatBoxSendParams {
  readonly editorTest?: NonNullable<ChatBoxProps['extensions']>['editorTest'];
  readonly deps: SendDeps;
  readonly getInternalToolsForSend?: () => Promise<readonly string[]>;
  /** Model settings the composer resolved; forwarded as the turn's `llm_settings`. */
  readonly llmSettings?: Readonly<Record<string, unknown>> | undefined;
  /**
   * The selected model. Passed as the object rather than a pre-read name so the
   * optional chain lives here — reading it at the ChatBox call site pushed that
   * component over its complexity budget.
   */
  readonly model?: { readonly name?: string | undefined } | null | undefined;
  readonly setChatHistory: (
    updater: (prev: readonly ChatMessage[]) => readonly ChatMessage[],
  ) => void;
  /**
   * The conversation on screen. The transport closes a stream whose owning
   * conversation is no longer this one and drops its frames — without it, a
   * run started in conversation A keeps writing into whichever conversation
   * the (never-unmounted) ChatBox switches to (#328).
   */
  readonly conversationUuid?: string | undefined;
  readonly projectId: string | number | undefined;
  readonly projectIdString: string | undefined;
  /**
   * The agents/test-panel host, which seeds no ad-hoc participants.
   *
   * It does NOT pick the execution contract any more: the sole `<ChatBox>`
   * call site never sets it, so the contract has to come from the participant
   * the turn addresses (`resolveStartContract`).
   */
  readonly isAgentsPage?: boolean | undefined;
  /** The signed-in user, for the ad-hoc turn's `user` participant. */
  readonly userId?: string | undefined;
  readonly activeParticipant?: unknown;
  readonly participants?: readonly unknown[] | undefined;
  readonly userName?: string | undefined;
  readonly userAvatar?: string | undefined;
  /** Graph frames, for a host that also renders a run timeline (the pipeline editor's canvas). Passed straight to `useChatStreamTransport`, which owns the WHICH-frames filter (`shouldForwardAgentEvent`); no second gate here, which would only be a copy that could drift. */
  readonly onAgentEvent?: ((frame: ExecutionEventData) => void) | undefined;
}

/** @public */
export interface UseChatBoxSendResult {
  /**
   * Start the run over REST and subscribe to its stream. `started` ⇒ this
   * transport owns the run and `chat_predict` must NOT also be emitted.
   */
  readonly startStreamedExecution: (params: {
    readonly conversationUuid: string;
    readonly payload: Record<string, unknown>;
  }) => Promise<StreamStartOutcome>;
  /**
   * Resume a PAUSED run over REST and re-subscribe to its stream.
   *
   * `started` ⇒ the route accepted the resume and `chat_continue_predict`
   * must NOT also be emitted; a second resume runs the agent twice.
   */
  readonly continueStreamedExecution: (params: {
    readonly conversationUuid: string;
    readonly contract: string;
    readonly body: Record<string, unknown>;
  }) => Promise<StreamStartOutcome>;
  readonly regenerateStreamedExecution: (params: {
    readonly messageId: string;
    readonly questionId: string;
    readonly question: string;
    readonly updatedItems?: readonly unknown[] | undefined;
  }) => Promise<StreamStartOutcome>;
  readonly isStreaming: boolean;
  /**
   * The user pressed Stop: cancel the run server-side and close its stream.
   * A no-op when this transport does not own the current run, so it is safe to
   * call alongside the socket-era `stopStreaming`.
   */
  readonly stopStreamedExecution: () => void;
  readonly createConversationForSend: (
    question: string,
  ) => Promise<
    { readonly id?: string | number; readonly uuid?: string } | undefined
  >;
  readonly uploadAttachmentsForSend: (
    conversationId: string | number,
    files: readonly File[],
  ) => Promise<{
    readonly success: boolean;
    readonly uploaded: UploadAttachmentsOutcome["uploaded"];
  }>;
}

export function useChatBoxSend(
  params: UseChatBoxSendParams,
): UseChatBoxSendResult {
  const { setChatHistory, projectId, projectIdString, isAgentsPage, getInternalToolsForSend } = params;
  const modelName = resolveSendModelName(params.llmSettings, params.model?.name);
  const target = useMemo(
    () => resolveTargetParticipant(params.activeParticipant, params.participants),
    [params.activeParticipant, params.participants],
  );
  const onContextChanged = contextManagementApi.useRefreshStatus();
  const transport = useChatStreamTransport({
    onContextChanged,
    setChatHistory,
    ...(params.conversationUuid !== undefined
      ? { conversationUuid: params.conversationUuid }
      : {}),
    context: buildChatStreamContext(params),
    onAgentEvent: params.onAgentEvent,
  });
  const { startDetailed, resume, resumeDetailed, regenerateDetailed } = transport;
  const requireTestTransport = useEditorTestTransport(params.editorTest, transport);

  const startStreamedExecution = useCallback(
    async ({
      conversationUuid,
      payload,
    }: {
      readonly conversationUuid: string;
      readonly payload: Record<string, unknown>;
    }): Promise<StreamStartOutcome> => {
      if (projectId === undefined) return requireTestTransport(NO_STREAM_TRANSPORT);
      const toolsFailure = await internalToolsSaveFailure(getInternalToolsForSend);
      if (toolsFailure) return toolsFailure;
      // The contract comes from the PARTICIPANT this turn addresses, never
      // from a page flag. The sole `<ChatBox>` call site passes no
      // `isAgentsPage`. Deriving it from that flag therefore sent every turn
      // — including one addressed to an agent — as `agent.execute.adhoc.v1`.
      // That resolver joins on `entity_name='dummy'` and answers 422 for an
      // agent participant.
      const isApplicationTurn =
        resolveStartContract(target) === conversationApi.contracts.application;
      if (
        (target as { readonly entity_name?: unknown } | null | undefined)
          ?.entity_name === "user"
      )
        return requireTestTransport(NO_STREAM_TRANSPORT);
      const targetParticipantId = positiveParticipantId(
        (target as { readonly id?: unknown } | null | undefined)?.id,
      );
      const body = buildStartBody({
        mcpTokens: await getExecutionTokens(projectIdString),
        conversationUuid,
        projectId: projectIdString,
        payload,
        llmSettings: params.llmSettings,
        modelName,
        isApplicationTurn,
        participantId:
          (isApplicationTurn
            ? positiveParticipantId(payload["participant_id"])
            : undefined) ?? targetParticipantId,
      });
      // No body can satisfy this contract (an agent turn with no addressable
      // participant). The socket fallback takes it rather than a POST that is
      // certain to be refused.
      if (body === undefined) return requireTestTransport(NO_STREAM_TRANSPORT);
      return requireTestTransport(await startDetailed({
        projectId,
        conversationUuid,
        contract: resolveStartContract(target),
        body,
      }));
    },
    [
      requireTestTransport,
      startDetailed,
      projectId,
      projectIdString,
      params.llmSettings,
      getInternalToolsForSend,
      modelName,
      target,
    ],
  );

  const continueStreamedExecution = useCallback(
    async ({
      conversationUuid,
      contract,
      body,
    }: {
      readonly conversationUuid: string;
      readonly contract: string;
      readonly body: Record<string, unknown>;
    }): Promise<StreamStartOutcome> => {
      if (projectId === undefined) return requireTestTransport(NO_STREAM_TRANSPORT);
      if (contract === 'agent.continue.static.v1') {
        return requireTestTransport(await resumeDetailed({ projectId, conversationUuid, contract, body }));
      }
      const resumed = await resume({
        projectId,
        conversationUuid,
        contract,
        body,
      });
      return requireTestTransport(resumed ? STREAM_STARTED : NO_STREAM_TRANSPORT);
    },
    [resume, resumeDetailed, projectId, requireTestTransport],
  );

  const regenerateStreamedExecution = useCallback(
    async (input: {
      readonly messageId: string;
      readonly questionId: string;
      readonly question: string;
      readonly updatedItems?: readonly unknown[] | undefined;
    }): Promise<StreamStartOutcome> => {
      if (projectId === undefined || params.conversationUuid === undefined)
        return requireTestTransport(NO_STREAM_TRANSPORT);
      const toolsFailure = await internalToolsSaveFailure(getInternalToolsForSend);
      if (toolsFailure) return toolsFailure;
      const isApplicationTurn =
        resolveStartContract(target) === conversationApi.contracts.application;
      const body = buildRegenerateBody({
        mcpTokens: await getExecutionTokens(projectIdString),
        conversationUuid: params.conversationUuid,
        projectId: projectIdString,
        responseMessageId: input.messageId,
        questionId: input.questionId,
        question: input.question,
        llmSettings: params.llmSettings,
        modelName,
        isApplicationTurn,
        participantId: positiveParticipantId(
          (target as { readonly id?: unknown } | null | undefined)?.id,
        ),
        ...(input.updatedItems !== undefined
          ? { updatedItems: input.updatedItems }
          : {}),
      });
      if (body === undefined) return requireTestTransport(NO_STREAM_TRANSPORT);
      // Verbatim, not collapsed to a boolean — see `regenerateStreamedWithRetry`
      // (`useChatBoxHandlers.regenerate.ts`) for the one refusal it carries.
      return requireTestTransport(await regenerateDetailed({
        projectId,
        conversationUuid: params.conversationUuid,
        responseMessageId: input.messageId,
        body,
      }));
    },
    [
      regenerateDetailed,
      requireTestTransport,
      projectId,
      projectIdString,
      params.conversationUuid,
      target,
      params.llmSettings,
      getInternalToolsForSend,
      modelName,
    ],
  );

  const { deps } = params;
  const createConversationForSend = useCallback(
    async (question: string) => {
      if (params.editorTest) throw new Error('A validated Test context is required.');
      const internalTools = await getInternalToolsForSend?.();
      const created = await deps.createConversation({
        name:
          question.slice(0, 50) ||
          t("widgets.chatBox.defaultConversationName", "New Chat"),
        isPrivate: true,
        meta: creationMeta(params.llmSettings, internalTools),
        ...(!isAgentsPage && modelName ? {
          participants: adhocParticipants({ userId: params.userId, modelName, llmSettings: params.llmSettings }),
        } : {}),
      });
      if (!created) return undefined;

      if (!isAgentsPage && !modelName) {
        // The conversation still exists when no model is available. Its blank
        // responder cannot supply model settings for a REST turn.
        console.warn(
          "[useChatBoxSend] no model is selected: created an ad-hoc conversation with no model settings, so its REST turns cannot resolve",
        );
      }
      return pickIdAndUuid(created);
    },
    [
      deps,
      params.editorTest,
      isAgentsPage,
      modelName,
      params.userId,
      params.llmSettings,
      getInternalToolsForSend,
    ],
  );

  const uploadAttachmentsForSend = useCallback(
    async (conversationId: string | number, files: readonly File[]) => {
      const cfg = getConfig();
      if (cfg.status !== "ok" || projectId === undefined)
        return { success: true, uploaded: [] };
      const outcome = await deps.uploadAttachments({
        baseUrl: cfg.config.vite_server_url,
        projectId: String(projectId),
        conversationId: String(conversationId),
        attachments: files,
      });
      return { success: outcome.success, uploaded: outcome.uploaded };
    },
    [deps, projectId],
  );

  return {
    startStreamedExecution,
    continueStreamedExecution,
    regenerateStreamedExecution,
    isStreaming: transport.isStreaming,
    stopStreamedExecution: transport.stop,
    createConversationForSend,
    uploadAttachmentsForSend,
  };
}

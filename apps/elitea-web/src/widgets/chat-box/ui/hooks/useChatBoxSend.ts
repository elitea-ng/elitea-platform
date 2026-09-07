/** Binds ChatBox send, continuation, and regeneration to the REST/SSE transport. */
import { useCallback } from "react";

import { useChatStreamTransport, type ChatMessage } from "@/features/chat-messages";
import { conversationApi } from "@/entities/conversation";
import { useAddParticipantMutation } from "@/entities/participant";
import { getExecutionTokens } from "@/features/mcps";
import type { useUploadAttachments } from "@/entities/conversation";

// Derived from the barrel-exported hook rather than deep-imported from its
// source file. `entities/conversation/index.ts` documents a 20-slot export cap
// and deliberately leaves the ~15 narrow param/result types out of it, telling
// consumers to import them from the concrete file — but that advice only holds
// WITHIN the slice. This widget is a different slice, so the deep path is a
// no-deep-slice-import-cross-slice violation (dependency-cruiser, "Layer +
// cycle gate"). Deriving keeps the single public entry point without spending
// two of the cap's remaining slots on types only this call site needs.
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
  executionStepsLimit,
  positiveParticipantId,
  resolveStartContract,
  resolveTargetParticipant,
} from "./useChatBoxSend.helpers";

/** The conversation-lifecycle and attachment-upload slices this hook adapts. */
interface SendDeps {
  readonly createConversation: (input: {
    name: string;
    isPrivate: boolean;
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
  readonly deps: SendDeps;
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
  const { setChatHistory, projectId, projectIdString, isAgentsPage } = params;
  const transport = useChatStreamTransport({
    setChatHistory,
    ...(params.conversationUuid !== undefined
      ? { conversationUuid: params.conversationUuid }
      : {}),
    context: buildChatStreamContext(params),
    onAgentEvent: params.onAgentEvent,
  });
  const { startDetailed, resume, regenerateDetailed } = transport;

  const startStreamedExecution = useCallback(
    async ({
      conversationUuid,
      payload,
    }: {
      readonly conversationUuid: string;
      readonly payload: Record<string, unknown>;
    }): Promise<StreamStartOutcome> => {
      // No project ⇒ no route to POST to. Reporting no-transport keeps the
      // socket fallback rather than silently sending nothing.
      if (projectId === undefined) return NO_STREAM_TRANSPORT;
      // The contract comes from the PARTICIPANT this turn addresses, never
      // from a page flag. The sole `<ChatBox>` call site passes no
      // `isAgentsPage`. Deriving it from that flag therefore sent every turn
      // — including one addressed to an agent — as `agent.execute.adhoc.v1`.
      // That resolver joins on `entity_name='dummy'` and answers 422 for an
      // agent participant.
      const target = resolveTargetParticipant(
        params.activeParticipant,
        params.participants,
      );
      const isApplicationTurn =
        resolveStartContract(target) === conversationApi.contracts.application;
      if (
        (target as { readonly entity_name?: unknown } | null | undefined)
          ?.entity_name === "user"
      )
        return NO_STREAM_TRANSPORT;
      const targetParticipantId = positiveParticipantId(
        (target as { readonly id?: unknown } | null | undefined)?.id,
      );
      const body = buildStartBody({
        mcpTokens: await getExecutionTokens(projectIdString),
        conversationUuid,
        projectId: projectIdString,
        payload,
        llmSettings: params.llmSettings,
        modelName: params.model?.name,
        isApplicationTurn,
        participantId:
          (isApplicationTurn
            ? positiveParticipantId(payload["participant_id"])
            : undefined) ?? targetParticipantId,
      });
      // No body can satisfy this contract (an agent turn with no addressable
      // participant). The socket fallback takes it rather than a POST that is
      // certain to be refused.
      if (body === undefined) return NO_STREAM_TRANSPORT;
      return startDetailed({
        projectId,
        conversationUuid,
        contract: resolveStartContract(target),
        body,
      });
    },
    [
      startDetailed,
      projectId,
      projectIdString,
      params.llmSettings,
      params.model,
      params.activeParticipant,
      params.participants,
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
      // No project ⇒ no route to POST to. Reporting no-transport keeps the
      // socket fallback rather than silently sending nothing.
      if (projectId === undefined) return NO_STREAM_TRANSPORT;
      const resumed = await resume({
        projectId,
        conversationUuid,
        contract,
        body,
      });
      return resumed ? STREAM_STARTED : NO_STREAM_TRANSPORT;
    },
    [resume, projectId],
  );

  const regenerateStreamedExecution = useCallback(
    async (input: {
      readonly messageId: string;
      readonly questionId: string;
      readonly question: string;
      readonly updatedItems?: readonly unknown[] | undefined;
    }): Promise<StreamStartOutcome> => {
      if (projectId === undefined || params.conversationUuid === undefined)
        return NO_STREAM_TRANSPORT;
      const target = resolveTargetParticipant(
        params.activeParticipant,
        params.participants,
      );
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
        modelName: params.model?.name,
        isApplicationTurn,
        participantId: positiveParticipantId(
          (target as { readonly id?: unknown } | null | undefined)?.id,
        ),
        ...(input.updatedItems !== undefined
          ? { updatedItems: input.updatedItems }
          : {}),
      });
      if (body === undefined) return NO_STREAM_TRANSPORT;
      // Verbatim, not collapsed to a boolean — see `regenerateStreamedWithRetry`
      // (`useChatBoxHandlers.regenerate.ts`) for the one refusal it carries.
      return regenerateDetailed({
        projectId,
        conversationUuid: params.conversationUuid,
        responseMessageId: input.messageId,
        body,
      });
    },
    [
      regenerateDetailed,
      projectId,
      projectIdString,
      params.conversationUuid,
      params.activeParticipant,
      params.participants,
      params.llmSettings,
      params.model,
    ],
  );

  const { deps } = params;
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const createConversationForSend = useCallback(
    async (question: string) => {
      const created = await deps.createConversation({
        name:
          question.slice(0, 50) ||
          t("widgets.chatBox.defaultConversationName", "New Chat"),
        isPrivate: true,
        ...(executionStepsLimit(params.llmSettings) !== undefined
          ? { meta: { steps_limit: executionStepsLimit(params.llmSettings) } }
          : {}),
      });
      if (!created) return undefined;

      // A plain model chat has to carry its own participants; an agent
      // conversation already has the agent as one, so this is scoped to the
      // ad-hoc path and to a chat that actually has a model to name.
      const modelName = params.model?.name;
      if (!isAgentsPage && !modelName) {
        // NOT silent, and not a refusal either. With no model there is nothing
        // to put in the `dummy` participant. Every REST turn this
        // conversation ever receives therefore resolves to no rows. The route
        // answers `422 unsupported_agent_execution`. The conversation is still
        // created because the socket path does not need that participant, so
        // refusing here would break a deployment whose socket works.
        //
        // `model` is null exactly when the project has no model catalogue or
        // the catalogue call failed. The user has to be told about that state.
        console.warn(
          "[useChatBoxSend] no model is selected: created an ad-hoc conversation with no `dummy` participant, so its REST turns cannot resolve",
        );
      }
      if (
        !isAgentsPage &&
        modelName &&
        created.id !== undefined &&
        projectId !== undefined
      ) {
        try {
          await addParticipants({
            projectId,
            conversationId: String(created.id),
            participants: adhocParticipants({
              userId: params.userId,
              modelName,
              llmSettings: params.llmSettings,
            }),
          });
        } catch (error) {
          // Not fatal to the send: the turn will fail its own admission with a
          // message, which is more useful than swallowing the question here.
          console.warn(
            "[useChatBoxSend] could not add ad-hoc participants:",
            error,
          );
        }
      }
      return pickIdAndUuid(created);
    },
    [
      deps,
      isAgentsPage,
      projectId,
      params.model,
      params.userId,
      params.llmSettings,
      addParticipants,
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

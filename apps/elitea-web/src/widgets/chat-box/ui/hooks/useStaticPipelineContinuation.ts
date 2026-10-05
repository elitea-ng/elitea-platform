/** Explicit static actions use durable REST only; observer attachment remains read-only. */
import { useCallback, useRef, useState } from "react";
import type { Dispatch, SetStateAction } from "react";
import {
  currentStaticPause,
  staticPauseKey,
  staticContinuationBody,
  consumeStaticPause,
  STATIC_CONTINUATION_CONTRACT,
  type ChatMessage,
} from "@/features/chat-messages";
import type { EditorTestRun } from "@/shared/api/generated/model";
import type { UseChatBoxSendResult } from "./useChatBoxSend";
export interface StaticContinuationParams {
  readonly messages: readonly ChatMessage[];
  readonly setChatHistory: Dispatch<SetStateAction<readonly ChatMessage[]>>;
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly continueExecution: UseChatBoxSendResult["continueStreamedExecution"];
  readonly restoredRun?:
    | {
        readonly run: EditorTestRun;
        readonly projectId: string | number;
        readonly conversationUuid: string;
      }
    | undefined;
  readonly isStreaming: boolean;
}
function submissionIdentity(
  params: StaticContinuationParams,
  key: string | undefined,
) {
  const pause = params.isStreaming
    ? undefined
    : currentStaticPause(params.messages, params.restoredRun?.run);
  if (
    !pause ||
    !params.conversationUuid ||
    JSON.stringify([
      params.projectId,
      params.conversationUuid,
      staticPauseKey(pause),
    ]) !== key
  )
    return;
  const projectId = Number(params.projectId);
  if (!Number.isSafeInteger(projectId) || projectId < 1) return;
  return { pause, projectId, conversationUuid: params.conversationUuid };
}
export function useStaticPipelineContinuation(
  params: StaticContinuationParams,
) {
  const latest = useRef(params);
  latest.current = params;
  const inFlight = useRef(false);
  const [busy, setBusy] = useState(false),
    [error, setError] = useState<string>();
  const restoredMatches =
    !params.restoredRun ||
    (String(params.restoredRun.projectId) === String(params.projectId) &&
      params.restoredRun.conversationUuid === params.conversationUuid);
  const pause =
    params.isStreaming || !restoredMatches
      ? undefined
      : currentStaticPause(params.messages, params.restoredRun?.run);
  const key = pause
    ? JSON.stringify([
        params.projectId,
        params.conversationUuid,
        staticPauseKey(pause),
      ])
    : undefined;
  const submit = useCallback(
    async (text: string, selected: readonly string[]) => {
      const active = latest.current;
      const identity = submissionIdentity(active, key);
      if (inFlight.current || !identity) return;
      const { pause: current, projectId } = identity;
      let body;
      try {
        body = staticContinuationBody(
          current,
          projectId,
          identity.conversationUuid,
          text,
          selected,
        );
      } catch (e) {
        setError(e instanceof Error ? e.message : "Invalid continuation.");
        return;
      }
      inFlight.current = true;
      setBusy(true);
      setError(undefined);
      try {
        const result = await active.continueExecution({
          conversationUuid: identity.conversationUuid,
          contract: STATIC_CONTINUATION_CONTRACT,
          body,
        });
        if (result.started) {
          active.setChatHistory((history) =>
            consumeStaticPause(history, current, selected),
          );
        } else {
          setError(
            result.reason === "rejected"
              ? result.message
              : "Durable static continuation is unavailable.",
          );
          if (result.reason === "rejected")
            active.setChatHistory((history) =>
              history.map((message) =>
                message.staticPause &&
                staticPauseKey(message.staticPause) === staticPauseKey(current)
                  ? { ...message, staticPause: undefined }
                  : message,
              ),
            );
        }
      } catch {
        setError("Continuation failed. Retry when the service is available.");
      } finally {
        inFlight.current = false;
        setBusy(false);
      }
    },
    [key],
  );
  return { pause, key, busy, error, submit };
}

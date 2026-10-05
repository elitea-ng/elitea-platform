import { StaticPipelineInventoryRootKind } from "@/shared/api/generated/model";
import { normalizeExecutionHierarchy } from "./executionHierarchy";
/** Static pauses settle controls, without inventing HITL or terminal events. */
import {
  createAssistantMessage,
  findTarget,
  replaceAt,
  type ChatStreamContext,
} from "./chatStreamShared";
import { staticPauseBinding } from "./staticPipelinePause";
import type { ChatMessage } from "./convertMessagesToChatHistory.types";
import type { ChatStreamFrame } from "./chatStreamFrame";
function isChildFrame(frame: ChatStreamFrame): boolean {
  const hierarchy = normalizeExecutionHierarchy(
    frame.response_metadata,
    frame.response_metadata?.metadata,
    frame.response_metadata?.tool_meta?.metadata,
  );
  return (
    hierarchy.parent_agent_path.length > 0 ||
    Boolean(hierarchy.parent_agent_name)
  );
}
export function staticPauseFromFrame(frame: ChatStreamFrame) {
  if (frame.type !== "full_message" || isChildFrame(frame)) return;
  const metadata = frame.response_metadata;
  const app = metadata?.["application_details"] as
    | { agent_type?: unknown; version_details?: { agent_type?: unknown } }
    | undefined;
  const root = metadata?.["pipeline_static_v1"],
    tools = metadata?.["pipeline_static_tools_v1"];
  const outer = app?.agent_type,
    version = app?.version_details?.agent_type;
  if (outer !== undefined && version !== undefined && outer !== version) return;
  const kind = StaticPipelineInventoryRootKind.safeParse(outer ?? version);
  if (!kind.success || (root !== undefined && kind.data !== "pipeline")) return;
  return staticPauseBinding(
    frame.message_id,
    frame.execution_generation,
    metadata?.thread_id,
    root,
    tools,
  );
}
export function applyStaticPauseFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  context: ChatStreamContext,
): readonly ChatMessage[] {
  if (isChildFrame(frame)) return history;
  const index = findTarget(history, frame),
    current = history[index];
  const generation = frame.execution_generation;
  if (
    current?.executionGeneration &&
    generation &&
    generation !== current.executionGeneration &&
    frame.type !== "agent_start" &&
    frame.type !== "start_task"
  )
    return history;
  if (frame.type === "agent_start" || frame.type === "start_task") {
    return current
      ? replaceAt(history, index, {
          staticPause: undefined,
          ...(generation ? { executionGeneration: generation } : {}),
        })
      : history;
  }
  if (frame.type === "full_message") {
    const binding = staticPauseFromFrame(frame);
    if (!binding)
      return current?.staticPause
        ? replaceAt(history, index, { staticPause: undefined })
        : history;
    const update = {
      staticPause: binding,
      executionGeneration: binding.generation,
      threadId: binding.threadId,
      isStreaming: false,
      isLoading: false,
      isRegenerating: false,
    };
    return current
      ? replaceAt(history, index, update)
      : [...history, { ...createAssistantMessage(frame, context), ...update }];
  }
  if (
    current?.staticPause &&
    [
      "agent_llm_start",
      "agent_response",
      "pipeline_finish",
      "error",
      "exception",
      "task_failed",
    ].includes(frame.type ?? "")
  )
    return replaceAt(history, index, { staticPause: undefined });
  return history;
}

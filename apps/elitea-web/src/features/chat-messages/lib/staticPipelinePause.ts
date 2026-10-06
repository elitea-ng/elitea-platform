/** Browser projections of server-owned, original static occurrences. */
import {
  StaticPipelinePauseProof,
  StaticPipelineToolInventory,
  StaticPipelineRootContinuation,
  StaticPipelineToolsContinuation,
  type StaticPipelineLeafDecision,
} from "@/shared/api/generated/model";
import type { ChatMessage } from "./convertMessagesToChatHistory.types";

import type { StaticPauseBinding } from './staticPipelinePause.types';
export const STATIC_CONTINUATION_CONTRACT = "agent.continue.static.v1";
const utf8 = (value: string): number => new TextEncoder().encode(value).length;
const identity = (value: unknown, max: number): value is string =>
  typeof value === "string" &&
  value.length > 0 &&
  utf8(value) <= max &&
  !value.includes("\0");
const node = (value: string): boolean =>
  identity(value, 128) && /^[a-zA-Z0-9_.:-]+$/.test(value);
const uuid = (value: unknown): value is string =>
  typeof value === "string" &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
function bounded(value: unknown, max: number): boolean {
  try {
    return utf8(JSON.stringify(value)) <= max;
  } catch {
    return false;
  }
}
function keys(value: unknown, allowed: readonly string[]): boolean {
  return (
    typeof value === "object" &&
    value !== null &&
    !Array.isArray(value) &&
    Object.keys(value).every((key) => allowed.includes(key))
  );
}
function proof(
  raw: unknown,
  thread: string,
): StaticPipelinePauseProof | undefined {
  if (
    !bounded(raw, 16384) ||
    !keys(raw, [
      "revision",
      "pause_id",
      "checkpoint_id",
      "kind",
      "node_name",
      "definition_digest",
      "node_digest",
      "pending_nodes",
      "step",
      "descendant_path",
    ])
  )
    return;
  const rawPath = (raw as Record<string, unknown>)["descendant_path"];
  if (
    !Array.isArray(rawPath) ||
    !rawPath.every((hop) =>
      keys(hop, ["node_name", "thread_id", "checkpoint_id"]),
    )
  )
    return;
  const parsed = StaticPipelinePauseProof.safeParse(raw);
  if (!parsed.success) return;
  const p = parsed.data;
  if (
    !node(p.node_name) ||
    !identity(p.checkpoint_id, 256) ||
    !p.pending_nodes.every(node) ||
    new Set(p.pending_nodes).size !== p.pending_nodes.length
  )
    return;
  if (
    p.kind === "before" &&
    (p.pending_nodes.length !== 1 || p.pending_nodes[0] !== p.node_name)
  )
    return;
  let parent = thread;
  for (const hop of p.descendant_path) {
    if (
      !keys(hop, ["node_name", "thread_id", "checkpoint_id"]) ||
      !node(hop.node_name) ||
      !identity(hop.thread_id, 1024) ||
      !identity(hop.checkpoint_id, 256) ||
      hop.thread_id !== `${parent}/${hop.node_name}`
    )
      return;
    parent = hop.thread_id;
  }
  return p;
}
export function staticPauseBinding(
  messageId: unknown,
  generation: unknown,
  thread: unknown,
  root: unknown,
  tools: unknown,
): StaticPauseBinding | undefined {
  if (
    !uuid(messageId) ||
    !uuid(generation) ||
    !identity(thread, 256) ||
    (root !== undefined && tools !== undefined)
  )
    return;
  if (root !== undefined) {
    const p = proof(root, thread);
    return p
      ? { messageId, generation, threadId: thread, kind: "root", proof: p }
      : undefined;
  }
  if (
    tools === undefined ||
    !bounded(tools, 131072) ||
    !keys(tools, ["revision", "pauses"])
  )
    return;
  const rawPauses = (tools as Record<string, unknown>)["pauses"];
  if (
    !Array.isArray(rawPauses) ||
    !rawPauses.every((pause) =>
      keys(pause, [
        "tool_call_id",
        "child_thread_id",
        "original_batch_event_id",
        "original_ordinal",
        "proof",
      ]),
    )
  )
    return;
  const parsed = StaticPipelineToolInventory.safeParse(tools);
  if (!parsed.success) return;
  const ids = new Set<string>(),
    calls = new Set<string>(),
    ordinals = new Set<string>();
  for (const [index, pause] of parsed.data.pauses.entries()) {
    if (
      !keys(pause, [
        "tool_call_id",
        "child_thread_id",
        "original_batch_event_id",
        "original_ordinal",
        "proof",
      ]) ||
      !identity(pause.tool_call_id, 512) ||
      !identity(pause.child_thread_id, 256) ||
      !identity(pause.original_batch_event_id, 512) ||
      !proof(
        (rawPauses[index] as Record<string, unknown>)["proof"],
        pause.child_thread_id,
      )
    )
      return;
    const call = JSON.stringify([pause.child_thread_id, pause.tool_call_id]),
      ordinal = JSON.stringify([
        pause.original_batch_event_id,
        pause.original_ordinal,
      ]);
    if (
      ids.has(pause.proof.pause_id) ||
      calls.has(call) ||
      ordinals.has(ordinal)
    )
      return;
    ids.add(pause.proof.pause_id);
    calls.add(call);
    ordinals.add(ordinal);
  }
  return {
    messageId,
    generation,
    threadId: thread,
    kind: "tools",
    inventory: parsed.data,
  };
}
export function currentStaticPause(
  messages: readonly ChatMessage[],
  receipt?: {
    readonly response_message_id: string;
    readonly execution_generation: string;
    readonly can_control: boolean;
    readonly phase: string;
  },
): StaticPauseBinding | undefined {
  const latest = messages.at(-1);
  const pause = latest?.staticPause;
  if (
    !latest ||
    latest.role !== "assistant" ||
    latest.exception ||
    latest.isStreaming ||
    latest.isLoading ||
    !pause ||
    latest.id !== pause.messageId ||
    latest.executionGeneration !== pause.generation ||
    latest.threadId !== pause.threadId
  )
    return;
  if (
    receipt &&
    (!receipt.can_control ||
      receipt.phase !== "PAUSED" ||
      receipt.response_message_id !== pause.messageId ||
      receipt.execution_generation !== pause.generation)
  )
    return;
  return pause;
}
export function staticPauseKey(pause: StaticPauseBinding): string {
  return JSON.stringify([
    pause.messageId,
    pause.generation,
    pause.threadId,
    pause.kind,
    pause.kind === "root"
      ? pause.proof.pause_id
      : pause.inventory.pauses.map((p) => p.proof.pause_id),
  ]);
}
/** Only public occurrence selectors enter the request. Main owns checkpoint/version selection. */
export function staticContinuationBody(
  pause: StaticPauseBinding,
  projectId: number,
  conversationUuid: string,
  text: string,
  selected: readonly string[],
) {
  if (!identity(text, 8192) || !text.trim())
    throw new Error("Enter a continuation message (at most 8 KiB).");
  const base = {
    project_id: projectId,
    conversation_uuid: conversationUuid,
    message_id: pause.messageId,
    thread_id: pause.threadId,
  };
  if (pause.kind === "root")
    return StaticPipelineRootContinuation.strict().parse({
      ...base,
      static_pause_id: pause.proof.pause_id,
      user_input: text,
    });
  if (!selected.length || new Set(selected).size !== selected.length)
    throw new Error("Select at least one paused pipeline.");
  const decisions: StaticPipelineLeafDecision[] = selected.map((id) => {
    const leaf = pause.inventory.pauses.find((p) => p.proof.pause_id === id);
    if (!leaf) throw new Error("The selected pause is no longer current.");
    return {
      pause_id: id,
      child_thread_id: leaf.child_thread_id,
      tool_call_id: leaf.tool_call_id,
      action: "continue",
      value: text,
    };
  });
  if (decisions.length * utf8(text) > 65536)
    throw new Error("Selected continuation messages exceed 64 KiB.");
  return StaticPipelineToolsContinuation.strict().parse({
    ...base,
    static_decisions: decisions,
  });
}
/** Clear only consumed static leaves; dynamic cards and unselected occurrences stay unchanged. */
export function consumeStaticPause(
  messages: readonly ChatMessage[],
  pause: StaticPauseBinding,
  selected: readonly string[],
): readonly ChatMessage[] {
  return messages.map((message) => {
    if (
      !message.staticPause ||
      staticPauseKey(message.staticPause) !== staticPauseKey(pause)
    )
      return message;
    if (pause.kind === "root") return { ...message, staticPause: undefined };
    const pauses = pause.inventory.pauses.filter(
      (p) => !selected.includes(p.proof.pause_id),
    );
    return {
      ...message,
      staticPause: pauses.length
        ? { ...pause, inventory: { ...pause.inventory, pauses } }
        : undefined,
    };
  });
}

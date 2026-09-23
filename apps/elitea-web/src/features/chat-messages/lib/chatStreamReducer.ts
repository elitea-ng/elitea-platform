/**
 * lib/chatStreamReducer.ts — the chat streaming reducer (issue #93, Surface B).
 *
 * PORT STATUS — COMPLETE. The baseline reducer is
 * `EliteaUI/src/components/Chat/hooks.js:391-1581`: 1,191 lines, 34 switch
 * cases. Every one of them is now accounted for, across seven slices: core
 * streaming, the tool lifecycle, thinking steps, interrupts, graph events,
 * swarm, and summaries. The sequence a live stack actually emits is:
 *
 *   agent_start → agent_on_transitional_edge → agent_llm_start
 *     → agent_llm_chunk* → agent_llm_end → agent_response
 *     → partial_message → full_message → pipeline_finish
 *
 * STATE-INERT BY DESIGN, which is a different thing from unported: the
 * `agent_on_*` graph frames drive the pipeline flow editor's node highlighting
 * and touch no message at all — the baseline's five case labels contain nothing
 * but the forward call. They fall through to `default` here on purpose, and
 * `agentGraphEvents.shouldForwardAgentEvent` carries the half a caller owes
 * them. `freeform` is inert for the same reason: the baseline's case is a bare
 * `break`, and the three raw `agent_swarm_*` frames are inert because the
 * baseline says outright that swarm work renders from `swarm_child_message`
 * alone.
 *
 * SIDE EFFECTS ARE NOT PORTED, and that is a boundary rather than an omission.
 * The baseline's tool cases also fire Google Analytics events, write
 * `window.__lastToolMetaFull`, and call `McpAuthHelpers.setSessionId` when a
 * tool reports an MCP session. A pure reducer cannot do any of that. The
 * metadata those effects read is preserved verbatim on `toolMeta`, so the hook
 * that eventually drives this reducer can perform them from the same frame —
 * see `mcpSessionFromFrame`, which extracts exactly that pair for a caller.
 *
 * SHAPE DEVIATION from the baseline, applied throughout: the baseline MUTATES a
 * message object and calls `setChatHistory` for its side effects, reading refs
 * for participants and the active participant. `ChatMessage` here is deeply
 * readonly, so this is a pure `(state, frame) => state` reducer — no refs, no
 * hooks, no I/O. That is what makes it testable against recorded frames without
 * a socket, a server, or a React tree, and it is why the port is worth doing
 * before the transport swap rather than after.
 *
 * FILE LAYOUT, and the honest reason for it: the reducer was one 1,181-line
 * module and §3.5's file-length budget caps a file at 400 lines with no warning
 * tier, so the one switch is now one sub-reducer per frame FAMILY —
 * `chatStreamTurnFrames`, `chatStreamToolFrames`, `chatStreamThinkingFrames`,
 * `chatStreamInterruptFrames`, `chatStreamSwarmFrames`,
 * `chatStreamSummaryFrames`, `chatStreamMessageSyncFrames` — over the shared
 * primitives in `chatStreamShared`. Each sub-reducer returns `undefined` for a
 * frame it does not own, which is what keeps "handled, and deliberately a
 * no-op" (a returned `history`) distinguishable from "not this family's frame";
 * the families are disjoint, so the order they are offered the frame in does
 * not matter. This module keeps the public surface it always had —
 * `applyChatStreamFrame`, `ChatStreamContext`, `ToolAction`,
 * `mcpSessionFromFrame` — so no importer changed.
 */
import { isTurnTerminalFrame } from './chatStreamTurnEnd';
import { settleContextProgress } from './chatStreamToolAction';
import { findTarget, replaceAt, type ChatStreamContext } from './chatStreamShared';
import { reduceTurnFrame } from './chatStreamTurnFrames';
import { reduceToolFrame } from './chatStreamToolFrames';
import { reduceToolOutputChunkFrame } from './chatStreamToolOutputChunks';
import { reduceThinkingFrame } from './chatStreamThinkingFrames';
import { reduceInterruptFrame } from './chatStreamInterruptFrames';
import { reduceSwarmFrame } from './chatStreamSwarmFrames';
import { reduceContextFrame } from './chatStreamContextFrames';
import { reduceSummaryFrame } from './chatStreamSummaryFrames';
import { reduceMessageSyncFrame } from './chatStreamMessageSyncFrames';

import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ChatStreamFrame } from './chatStreamFrame';

export type { ChatStreamContext, ToolAction } from './chatStreamShared';
export { mcpSessionFromFrame } from './chatStreamToolFrames';

/**
 * Apply one streaming frame.
 *
 * Returns the SAME array reference when the frame changes nothing, so a caller
 * passing this to `setState` does not re-render on frames this slice ignores.
 */
export function applyChatStreamFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  context: ChatStreamContext = {},
): readonly ChatMessage[] {
  const type = frame.type;
  if (!type) return history;

  // Resolved ONCE, here, and handed to every family: `index === -1` means "no
  // message for this frame yet", which several cases read as "create one".
  // Re-deriving it per family would be the same call today and a divergence
  // the first time one of them narrowed the lookup. `type` is passed down for
  // the same reason plus a smaller one — it is the already-narrowed, non-empty
  // string the switches (and the error case's `exception` fallback) want.
  const index = findTarget(history, frame);

  const next = (
    reduceTurnFrame(history, frame, type, context, index) ??
    reduceToolFrame(history, frame, type, index) ??
    reduceToolOutputChunkFrame(history, frame, type, index) ??
    reduceThinkingFrame(history, frame, type, index) ??
    reduceInterruptFrame(history, frame, type, context, index) ??
    reduceSwarmFrame(history, frame, type, context) ??
    reduceContextFrame(history, frame, type, context, index) ??
    reduceSummaryFrame(history, frame, type, context, index) ??
    reduceMessageSyncFrame(history, frame, type, context) ??
    // Not yet ported (see the module doc). Returning the input reference is the
    // point: an unported frame must be inert, never a partial write.
    history
  );
  if (!isTurnTerminalFrame(frame)) return next;
  const target = findTarget(next, frame);
  const message = next[target];
  if (!message) return next;
  const toolActions = settleContextProgress(message.toolActions);
  return toolActions === message.toolActions ? next : replaceAt(next, target, { toolActions });
}

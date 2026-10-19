/**
 * lib/chatStreamInterruptFrames.ts — the frames that hand the turn to a human.
 *
 * Owns `agent_requires_confirmation` (the token-limit continue button) and
 * `agent_hitl_interrupt` (the three pause shapes), plus the single-pause raw
 * assembly only they use. It is its own file because `chatStreamReducer.ts`
 * broke the §3.5 400-line budget, which has no warning tier — nothing about
 * these two cases changed; they and their comments are the originals.
 */
import {
  firstAnswerableInterrupt,
  isParallelAggregateInterrupt,
  normalizeHitlInterrupt,
  parallelAggregateMemberIds,
  reconcileHitlInterrupts,
  type NormalizedHitlInterrupt,
} from './hitlInterrupts';
import { replaceAt, threadIdOf, type ChatStreamContext } from './chatStreamShared';
import { readParked } from './fanoutFrames';

import type { ChatMessage } from './convertMessagesToChatHistory';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';

/**
 * The one raw interrupt a single pause implies.
 *
 * A single pause does not send `hitl_interrupts`; its detail is split between
 * legacy top-level metadata and the nested `hitl_interrupt`, and reading only
 * one of the two loses either the routing fields or the tool detail the card
 * renders.
 */
function singlePauseRaw(frame: ChatStreamFrame): Record<string, unknown> {
  const responseMetadata = frame.response_metadata;
  const nested = responseMetadata?.hitl_interrupt ?? {};
  return {
    message: responseMetadata?.message,
    node_name: responseMetadata?.node_name,
    available_actions: responseMetadata?.available_actions,
    routes: responseMetadata?.routes,
    edit_state_key: responseMetadata?.edit_state_key,
    guardrail_type: nested['guardrail_type'],
    // The clarifying questions of an `ask_user` pause. Only the nested
    // interrupt carries them, and the card has no other source: without this
    // line the pause renders its question TEXT and no controls at all.
    questions: nested['questions'],
    tool_name: nested['tool_name'],
    toolkit_name: nested['toolkit_name'],
    toolkit_type: nested['toolkit_type'],
    action_label: nested['action_label'],
    tool_args: nested['tool_args'],
    policy_message: nested['policy_message'],
    interrupt_id: nested['interrupt_id'] ?? responseMetadata?.interrupt_id,
    tool_call_id: nested['tool_call_id'],
    resume_strategy: nested['resume_strategy'] ?? responseMetadata?.resume_strategy,
  };
}

/**
 * The pause-progress marker (contract `fanout-interrupt-decisions-v1.md`
 * §9.2, FU-227). The Worker stamps `response_metadata.pause_progress: true` on
 * every per-card pause frame (`output_delivery.rs` `stamp_pause_progress`,
 * `fanout_frames.rs` `interrupt_pending_event`) and never on the aggregate:
 * the root path's paused terminal frame (`finish_hitl`) and the parked
 * terminal frame (`fanout_parked_v1`). The pause batch therefore settles on
 * the FIRST pause frame of the response without the marker; no timer guesses
 * when the batch is over.
 */
const PAUSE_PROGRESS_MARKER = 'pause_progress';

/** Does this frame carry the parked execution's `fanout_parked_v1` (contract §8)? */
function isParkedFrame(frame: ChatStreamFrame): boolean {
  return readParked(frame.response_metadata?.['fanout_parked_v1']) !== undefined;
}

/**
 * A fan-out CHILD pause (Track-2 indexer shape): it stamps the child's own
 * thread and its parent's name into event metadata, and its siblings keep
 * streaming on the same connection.
 */
function isFanoutChildPause(frame: ChatStreamFrame): boolean {
  const hitlMeta = (frame.response_metadata?.metadata ?? {}) as Record<string, unknown>;
  const childThreadId = typeof hitlMeta['child_thread_id'] === 'string' ? hitlMeta['child_thread_id'] : '';
  return Boolean(hitlMeta['parent_agent_name'] && childThreadId);
}

/**
 * Is this pause frame the LAST frame of the run, or progress inside it?
 *
 * The worker ends a paused run with `agent_hitl_interrupt` or
 * `mcp_authorization_required` and emits no `pipeline_finish` and no
 * `agent_response`. The stream transport must release the connection on those
 * frames. If it does not, the composer stays disabled for the rest of the
 * session. The stream also holds a server admission slot.
 *
 * Neither type is terminal on its own, so a blanket type test would truncate a
 * live stream:
 *
 * - `mcp_authorization_required` is published per card as progress, then once
 *   as the aggregate. Only the aggregate carries `authorization_requests`.
 * - `agent_hitl_interrupt` is published per card as progress, then once as the
 *   aggregate (which also carries `authorization_requests` for a mixed pause).
 *   See `PAUSE_PROGRESS_MARKER` for why only a marked frame is progress.
 * - a legacy FAN-OUT CHILD pause keeps the stream open: its siblings still send
 *   frames on the SAME stream, and only the reconciled parent ends the run.
 *
 * The reducer reads this same predicate for the message's streaming state, so
 * "the run is parked" and "the observer is released" cannot disagree.
 */
export function isTerminalPauseFrame(frame: ChatStreamFrame): boolean {
  if (frame.response_metadata?.[PAUSE_PROGRESS_MARKER] === true) return false;
  // The parked terminal frame ends the run whatever its type (§8).
  if (isParkedFrame(frame)) return true;
  if (frame.type === SocketMessageType.McpAuthorizationRequired) {
    return Array.isArray(frame.response_metadata?.['authorization_requests']);
  }
  if (frame.type !== SocketMessageType.AgentHitlInterrupt) return false;
  return !isFanoutChildPause(frame);
}

/**
 * The raw interrupts a frame names, with the reconcile-only parallel aggregate
 * split out. A single pause does not always send `hitl_interrupts`; its one
 * card is then assembled from the legacy fields (`singlePauseRaw`).
 */
function frameInterrupts(frame: ChatStreamFrame): {
  readonly cards: readonly Record<string, unknown>[];
  readonly memberIds: readonly string[];
  /** The cards are the frame's own `hitl_interrupts` entries, each complete. */
  readonly listed: boolean;
} {
  const responseMetadata = frame.response_metadata;
  const listed = Array.isArray(responseMetadata?.hitl_interrupts) ? responseMetadata.hitl_interrupts : [];
  const nested = responseMetadata?.hitl_interrupt;
  // A frame whose only pause IS the aggregate (nested, or the metadata itself)
  // names no card: assembling one from its legacy fields would invent it.
  // The parked terminal frame names cards only by key: it is reconcile-only
  // (`applyFanoutParkedFrame`) and never synthesises a card of its own.
  const aggregateOnly = isParallelAggregateInterrupt(nested) || isParallelAggregateInterrupt(responseMetadata)
    || (nested === undefined && isParkedFrame(frame));
  const raws: readonly Record<string, unknown>[] =
    listed.length > 0 ? listed : aggregateOnly ? [] : [singlePauseRaw(frame)];
  const candidates: readonly (Readonly<Record<string, unknown>> | undefined)[] = [...listed, nested, responseMetadata];
  const aggregates = candidates.filter(
    (raw): raw is Readonly<Record<string, unknown>> => raw !== undefined && isParallelAggregateInterrupt(raw),
  );
  return {
    cards: raws.filter((raw) => !isParallelAggregateInterrupt(raw)),
    memberIds: aggregates.flatMap((raw) => parallelAggregateMemberIds(raw)),
    listed: listed.length > 0,
  };
}

/**
 * Reduce one interrupt frame, or return `undefined` for a frame this family
 * does not own so the dispatcher can offer it to the next one.
 */
export function reduceInterruptFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  context: ChatStreamContext,
  index: number,
): readonly ChatMessage[] | undefined {
  switch (type) {
    // The model stopped on its token limit rather than on an answer. The turn
    // is not finished — `requiresConfirmation` is what renders the continue
    // button that resumes it.
    case SocketMessageType.AgentRequiresConfirmation: {
      if (index === -1) return history;
      const current = history[index];
      if (!current) return history;
      const threadId = threadIdOf(frame);
      const buttonText = typeof frame.content === 'string' && frame.content ? frame.content : 'Continue';

      return replaceAt(history, index, {
        isLoading: false,
        // Mono chat keeps the same bubble streaming across the continue; a
        // multi-participant conversation must release the floor.
        isStreaming: Boolean(context.isMonoChatting),
        isRegenerating: false,
        // Only when the frame supplies one: the thread was already set by the
        // response or tool frames of THIS message, and blanking it would strand
        // the continue request with nowhere to resume.
        ...(threadId !== undefined ? { threadId } : {}),
        requiresConfirmation: {
          message: "Token limit reached mid-response. Press 'Continue' to see more.",
          buttonText,
        },
      });
    }

    // Execution paused for a human decision. Every shape folds into ONE list
    // by identity, so a card announced as progress and again in the aggregate
    // is one card, and the user's local queued/decided state survives both.
    case SocketMessageType.AgentHitlInterrupt: {
      if (index === -1) return history;
      const current = history[index];
      if (!current) return history;
      const responseMetadata = frame.response_metadata;
      const hitlMeta = (responseMetadata?.metadata ?? {}) as Record<string, unknown>;
      const childThreadId = typeof hitlMeta['child_thread_id'] === 'string' ? hitlMeta['child_thread_id'] : '';
      const isFanoutChild = isFanoutChildPause(frame);
      // A parked run is not streaming. Only a progress frame — siblings still
      // running behind it — keeps the live view open.
      const streamingState = {
        isStreaming: !isTerminalPauseFrame(frame),
        isLoading: false,
        isRegenerating: false,
      };

      const fallbackMessage =
        (typeof responseMetadata?.message === 'string' ? responseMetadata.message : '') || current.content;
      const { cards, memberIds, listed } = frameInterrupts(frame);
      // The event metadata overlays ONLY a card the frame synthesises from its
      // legacy fields, or a legacy fan-out child's. A listed entry is complete:
      // the aggregate clones the FIRST card's metadata, and overlaying it would
      // file a coordinator card under that card's sub-agent — live only, so a
      // reload would render it differently.
      const overlay = !listed || isFanoutChild ? { ...hitlMeta, child_thread_id: childThreadId, thread_id: childThreadId } : {};
      const build = (raw: Record<string, unknown>): NormalizedHitlInterrupt =>
        normalizeHitlInterrupt({ ...raw, message: raw['message'] ?? fallbackMessage }, overlay);
      const existing = (current.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
      // Always the array, including for one card: the resume path decides the
      // body from the size of the pending set, not from the array's presence.
      const hitlInterrupts = reconcileHitlInterrupts(existing, cards.map(build), memberIds);

      const threadId =
        current.threadId ??
        (typeof hitlMeta['thread_id'] === 'string' ? hitlMeta['thread_id'] : undefined) ??
        responseMetadata?.thread_id;

      // Content is deliberately NOT overwritten with the interrupt text: the
      // pause renders from the card, and a written-in "requires approval" line
      // would linger in the bubble after the user resumes.
      return replaceAt(history, index, {
        ...streamingState,
        hitlInterrupts,
        // Kept for consumers that read the singular field: the first card still
        // owed an answer.
        hitlInterrupt: firstAnswerableInterrupt(hitlInterrupts),
        // A fan-out child resumes on its own thread, carried per entry — parking
        // the whole message on whichever child paused last would misroute it.
        ...(!isFanoutChild && threadId !== undefined ? { threadId } : {}),
      });
    }

    default:
      return undefined;
  }
}

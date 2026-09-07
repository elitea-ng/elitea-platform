/**
 * The block-level operations every thinking frame is built from.
 *
 * All of them are NO-OPS when no block is open, which is the legacy
 * `updateThinkingBlock`'s early return. It matters: the send path opens a block
 * before it emits, so a step arriving with none open belongs to a run that has
 * already finished, and appending it would attach it to the previous answer.
 */
import {
  MAX_THINKING_STEPS_PER_RUN,
  isThinkingBlock,
  type ChatMessage,
  type ChatState,
  type ChatThinkingBlock,
  type ChatThinkingStep,
} from '../types';

/** Apply `updater` to the OPEN block, if there is one. */
export function updateActiveBlock(
  state: ChatState,
  updater: (block: ChatThinkingBlock) => ChatThinkingBlock,
): ChatState {
  const activeId = state.activeBlockId;
  if (!activeId) return state;

  let changed = false;
  const messages = state.messages.map((message): ChatMessage => {
    if (isThinkingBlock(message) && message.id === activeId) {
      changed = true;
      return updater(message);
    }
    return message;
  });
  // Identity is preserved when the block id names nothing, so a caller can tell
  // "nothing to do" from "did nothing".
  return changed ? { ...state, messages } : state;
}

/** Trim to the cap, keeping the MOST RECENT steps. */
export function capSteps(steps: readonly ChatThinkingStep[]): readonly ChatThinkingStep[] {
  return steps.length > MAX_THINKING_STEPS_PER_RUN
    ? steps.slice(steps.length - MAX_THINKING_STEPS_PER_RUN)
    : steps;
}

/** Append one step to the open block. */
export function appendStep(state: ChatState, step: ChatThinkingStep): ChatState {
  return updateActiveBlock(state, (block) => ({
    ...block,
    steps: capSteps([...block.steps, step]),
  }));
}

/**
 * Close the open block and forget it.
 *
 * Both terminal families do exactly this before they append their turn, and
 * doing it in one place is what stops one of them from leaving a block spinning
 * for ever — which is what the user sees if it is missed.
 */
export function closeActiveBlock(state: ChatState): ChatState {
  const closed = updateActiveBlock(state, (block) => ({ ...block, status: 'completed' }));
  return { ...closed, activeBlockId: null };
}

/**
 * A primitive rendered as text; anything else is NOT text.
 *
 * `String(someObject)` yields "[object Object]", which is a defect wearing a
 * value's clothes — it renders, it looks deliberate, and it says nothing. An
 * object reaching a text slot means the payload was not the shape the branch
 * expected, and the empty string is the honest answer.
 */
export function primitiveText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    return String(value);
  }
  return '';
}

/** Read a field off an unknown payload without asserting its shape. */
export function field(value: unknown, name: string): unknown {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)[name]
    : undefined;
}

/**
 * Parse the structured `{event, data}` envelope, or report that there is not
 * one.
 *
 * They are told apart by parsing and then checking for an `event` KEY — not by
 * whether the parse succeeded. Valid JSON with no `event` is not a structured
 * event and takes the plain path, which is why
 * `plain-metadata-json-without-event` is in the oracle: a reader that branched
 * on "did it parse" would render the raw JSON as a structured event of type
 * `undefined`.
 *
 * IT LIVES HERE, not in the one family that used to own it, because the POLL
 * ADAPTER reads the same envelope to lift answer tokens out of the event list
 * before the reducer ever sees them (`lib/framesFromChatPoll.ts`, issue #701).
 * Two spellings of "is this a structured event" would let one side route a
 * frame the other did not, and the symptom is an answer fragment rendered as a
 * thinking card — visible, wrong, and a failure nowhere.
 */
export function structuredEvent(raw: unknown): { event: string; data: unknown } | null {
  let parsed: unknown = raw;
  if (typeof raw === 'string') {
    try {
      parsed = JSON.parse(raw);
    } catch {
      return null;
    }
  }
  const name = field(parsed, 'event');
  return typeof name === 'string' && name !== ''
    ? { event: name, data: field(parsed, 'data') }
    : null;
}

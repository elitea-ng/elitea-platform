/**
 * `agent_thinking_step` and `agent_thinking_step_update`, which carry TWO
 * different payloads down one frame type.
 *
 * The modern shape is a STRUCTURED EVENT: `response_metadata.message` holds a
 * JSON string with `{event, data}`, and `event` names one of seven behaviours.
 * The legacy shape is plain text in the same place, or in `content`.
 *
 * They are told apart by parsing and then checking for an `event` KEY — not by
 * whether the parse succeeded. Valid JSON with no `event` is not a structured
 * event and takes the plain path, which is why `plain-metadata-json-without-event`
 * is in the oracle: a reader that branched on "did it parse" would render the
 * raw JSON as a structured event of type `undefined`.
 *
 * `structuredEvent` itself moved to ./shared when the poll adapter grew a
 * reader for the same envelope — see the note there.
 */
import {
  capSteps,
  appendStep,
  field,
  primitiveText,
  structuredEvent,
  updateActiveBlock,
} from './shared';
import type { ChatFrame, ChatState, ChatThinkingStep, ChatTodo } from '../types';

/** `eventData.items`, then `.todos`, then the payload itself — and [] if none is a list. */
function todosFrom(data: unknown): readonly ChatTodo[] {
  const candidate = field(data, 'items') ?? field(data, 'todos') ?? data;
  return Array.isArray(candidate) ? (candidate as readonly ChatTodo[]) : [];
}

function firstString(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === 'string' && value !== '') return value;
  }
  return '';
}

/**
 * A step id, as a string.
 *
 * The legacy code stored whatever the payload put in `id` — a number stayed a
 * number — and then matched `tool_end` against it with `===`. Storing it as a
 * string here is a TYPE narrowing, not a behaviour change: the lookup below
 * narrows the incoming id the same way, so a numeric id still merges with its
 * own start. The falsy test is the legacy `||`, which treats `0` and `''` as
 * "no id" exactly as before.
 */
function stepId(value: unknown): string {
  return value ? primitiveText(value) : '';
}

function reduceToolStart(state: ChatState, data: unknown, now: () => number): ChatState {
  return appendStep(state, {
    id: stepId(field(data, 'id')) || `tool-${now()}`,
    event: 'tool_start',
    data: {
      ...(typeof data === 'object' && data !== null ? data : {}),
      // The ALIAS is resolved into the card's data...
      tool: field(data, 'tool') ?? field(data, 'toolName'),
      input: field(data, 'input') ?? field(data, 'args') ?? '',
      output: '',
    },
    // ...but NOT into its label, which reads the raw `tool` only. A tool that
    // announced itself as `toolName` is therefore labelled "Calling: tool".
    // Faithful to the legacy code and recorded as such: it is a cosmetic gap in
    // one alias path, and diverging here would put this port's own text on a
    // card while claiming to reproduce the original.
    message: firstString(field(data, 'description')) || `Calling: ${firstString(field(data, 'tool')) || 'tool'}`,
  });
}

/**
 * `tool_end` MERGES into the card its `tool_start` left, matched by id.
 *
 * The merge is what makes one tool one card. An unmatched id appends its own
 * card instead of being dropped, because a tool whose start was missed still
 * produced output the user should see.
 */
function reduceToolEnd(state: ChatState, data: unknown, now: () => number): ChatState {
  const toolId = field(data, 'id');
  const payload = typeof data === 'object' && data !== null ? data : {};

  return updateActiveBlock(state, (block) => {
    // TWO ways to match, and both are the legacy ones. The right-hand side
    // compares RAW values, so a `tool_end` carrying no id at all matches the
    // first step whose data also carries none (undefined === undefined). That
    // is surprising, it is what shipped, and `order-tool-end-no-id` in the
    // oracle is there to keep it from being "cleaned up" by accident.
    const existingIndex = block.steps.findIndex(
      (step) => step.id === stepId(toolId) || field(step.data, 'id') === toolId,
    );

    if (existingIndex >= 0) {
      const steps = [...block.steps];
      steps[existingIndex] = mergeToolEnd(steps[existingIndex] as ChatThinkingStep, data, payload);
      return { ...block, steps: capSteps(steps) };
    }

    return {
      ...block,
      steps: capSteps([...block.steps, orphanToolEnd(toolId, data, payload, now)]),
    };
  });
}

/** The `tool_end` payload folded onto the card its `tool_start` left. */
function mergeToolEnd(
  existing: ChatThinkingStep,
  data: unknown,
  payload: object,
): ChatThinkingStep {
  return {
    ...existing,
    event: 'tool_end',
    data: {
      ...(typeof existing.data === 'object' && existing.data !== null ? existing.data : {}),
      ...payload,
      // The START's input wins: `tool_end` rarely repeats it, and letting an
      // absent one overwrite would erase what the tool was asked.
      input: field(existing.data, 'input') || field(data, 'input') || '',
      output: field(data, 'output') ?? field(data, 'result') ?? '',
      status: 'completed',
    },
  };
}

/** A `tool_end` whose start was never seen still gets a card of its own. */
function orphanToolEnd(
  toolId: unknown,
  data: unknown,
  payload: object,
  now: () => number,
): ChatThinkingStep {
  return {
    id: stepId(toolId) || `tool-${now()}`,
    event: 'tool_end',
    data: {
      ...payload,
      tool: field(data, 'tool') ?? field(data, 'toolName'),
      output: field(data, 'output') ?? field(data, 'result') ?? '',
      status: 'completed',
    },
    message: firstString(field(data, 'description')) || 'Tool completed',
  };
}

/**
 * Only the LATEST `llm_thinking` is shown.
 *
 * It is a live "the model is writing" chip, not a log line, so every earlier one
 * is removed rather than stacked. Removal is by EVENT, not by id: two chips with
 * different ids are still two claims about the same present moment.
 */
function reduceLlmThinking(state: ChatState, data: unknown, now: () => number): ChatState {
  return updateActiveBlock(state, (block) => ({
    ...block,
    steps: capSteps([
      ...block.steps.filter((step) => step.event !== 'llm_thinking'),
      {
        id: stepId(field(data, 'id')) || `llm-${now()}`,
        event: 'llm_thinking',
        data,
        message: firstString(field(data, 'message')) || 'Thinking...',
      },
    ]),
  }));
}

function reduceStructured(
  state: ChatState,
  event: string,
  data: unknown,
  now: () => number,
): ChatState {
  switch (event) {
    case 'todo_update':
      return { ...state, todos: todosFrom(data) };
    case 'tool_start':
      return reduceToolStart(state, data, now);
    case 'tool_end':
      return reduceToolEnd(state, data, now);
    case 'llm_thinking':
      return reduceLlmThinking(state, data, now);
    case 'thinking':
      return appendStep(state, {
        id: stepId(field(data, 'id')) || `think-${now()}`,
        event,
        data,
        message: firstString(field(data, 'message'), field(data, 'title')),
      });
    case 'status':
    case 'log':
      return appendStep(state, {
        id: `${event}-${now()}`,
        event,
        data,
        message: firstString(field(data, 'message')),
      });
    default:
      // An event this screen has no reading for is still SHOWN. The stream is
      // shared with the generation screen and gains names without warning; a
      // card saying something unfamiliar happened beats silence.
      return appendStep(state, {
        id: `${event}-${now()}`,
        event,
        data,
        message:
          firstString(field(data, 'message'), field(data, 'title')) || JSON.stringify(data),
      });
  }
}

/** The unstructured path: `response_metadata.message`, then `content`. */
function reducePlainText(state: ChatState, frame: ChatFrame, now: () => number): ChatState {
  const metadataMessage = frame.response_metadata?.['message'];
  const fromContent =
    typeof frame.content === 'object' && frame.content !== null
      ? field(frame.content, 'message')
      : frame.content;

  return appendStep(state, {
    id: `step-${now()}`,
    event: 'log',
    message: firstString(metadataMessage, fromContent) || 'Processing...',
    type: frame.type,
  });
}

export function reduceThinkingFrame(
  state: ChatState,
  frame: ChatFrame,
  now: () => number,
): ChatState {
  const structured = structuredEvent(frame.response_metadata?.['message']);
  return structured
    ? reduceStructured(state, structured.event, structured.data, now)
    : reducePlainText(state, frame, now);
}

/**
 * The DIRECT `todo_update` socket type, which deep research emits outside the
 * thinking envelope. Same destination, different route — `content.todos`, then
 * `content` itself.
 */
export function reduceDirectTodoUpdate(state: ChatState, frame: ChatFrame): ChatState {
  const candidate = field(frame.content, 'todos') ?? frame.content ?? [];
  return { ...state, todos: Array.isArray(candidate) ? (candidate as readonly ChatTodo[]) : [] };
}

import type { NodeEvent } from '@/features/chat/frames';

/**
 * Tool activity and pauses read from the agent progress frames (client
 * contract 1.1, the `x-elitea-client-frames` catalogue). Parsed loosely: a
 * field of the wrong type is treated as absent, unknown keys are ignored.
 *
 * Tool I/O is shown on demand and never persisted; in memory each step keeps
 * at most `MAX_TOOL_TEXT` of its inputs and of its output.
 */

/** In-memory cap per step for inputs and for output (8 KiB of UTF-16 text). */
export const MAX_TOOL_TEXT = 8 * 1024;
/** At most this many output chunks are taken per tool call (a larger declared total is shown as partial). */
export const MAX_TOOL_CHUNKS = 4096;
/** Longest tool, toolkit or agent name kept. */
const MAX_NAME = 200;

/** `waiting`: stopped by an approval pause (the tool has not run yet). */
export type ToolStatus = 'running' | 'done' | 'error' | 'needs_auth' | 'waiting';

export interface ToolStep {
  /** `tool_run_id`: the call's key. */
  id: string;
  name: string;
  /** `metadata.display_name`, when the tool has one. */
  displayName: string | null;
  toolkit: string | null;
  /** The sub-agent that ran it (`metadata.parent_agent_name`). */
  parentAgent: string | null;
  status: ToolStatus;
  startedAt: number | null;
  endedAt: number | null;
  /** The arguments as pretty JSON (may be sensitive: shown on demand). */
  inputs: string | null;
  inputsTruncated: boolean;
  output: string | null;
  outputTruncated: boolean;
  /** The step was built from an end frame whose start was lost, or its chunks are incomplete. */
  partial: boolean;
  error: string | null;
  /** Output chunks received so far (`agent_tool_output_chunk`), by index. */
  chunks: {
    total: number;
    received: Record<number, string>;
    /** Characters kept in `received` (a running count, not re-summed per frame). */
    kept: number;
  } | null;
}

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const asString = (v: unknown): string | null => (typeof v === 'string' && v !== '' ? v : null);
const asName = (v: unknown): string | null => {
  const s = asString(v)?.trim();
  return s ? s.slice(0, MAX_NAME) : null;
};
const asCount = (v: unknown): number | null => (typeof v === 'number' && Number.isSafeInteger(v) && v >= 0 ? v : null);

function parseTime(v: unknown): number | null {
  if (typeof v !== 'string') return null;
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : null;
}

/** Caps text to `MAX_TOOL_TEXT`; the flag says whether something was cut. */
export function capText(text: string): { text: string; truncated: boolean } {
  return text.length > MAX_TOOL_TEXT ? { text: text.slice(0, MAX_TOOL_TEXT), truncated: true } : { text, truncated: false };
}

/** Tool arguments or output as display text: strings as is, anything else as pretty JSON. */
export function displayValue(value: unknown): string | null {
  if (value === null || value === undefined) return null;
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value, null, 2) ?? null;
  } catch {
    return null;
  }
}

/** A JSON string's pretty form, or the text itself when it is not JSON. */
export function prettyJson(text: string): string {
  const trimmed = text.trim();
  if (!(trimmed.startsWith('{') || trimmed.startsWith('['))) return text;
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2);
  } catch {
    return text;
  }
}

function emptyStep(id: string, name: string): ToolStep {
  return {
    id,
    name,
    displayName: null,
    toolkit: null,
    parentAgent: null,
    status: 'running',
    startedAt: null,
    endedAt: null,
    inputs: null,
    inputsTruncated: false,
    output: null,
    outputTruncated: false,
    partial: false,
    error: null,
    chunks: null,
  };
}

function toolMeta(rm: Record<string, unknown>) {
  const meta = asRecord(rm.metadata) ?? asRecord(asRecord(rm.tool_meta)?.metadata) ?? {};
  return {
    name: asName(rm.tool_name) ?? asName(asRecord(rm.tool_meta)?.name) ?? 'tool',
    displayName: asName(meta.display_name),
    toolkit: asName(meta.toolkit_name),
    parentAgent: asName(meta.parent_agent_name),
  };
}

function withInputs(step: ToolStep, inputs: unknown): ToolStep {
  if (step.inputs !== null) return step;
  const text = displayValue(inputs);
  if (text === null) return step;
  const capped = capText(text);
  return { ...step, inputs: capped.text, inputsTruncated: capped.truncated };
}

/** The assembled output of complete chunks, in index order (null while incomplete). */
function assembled(chunks: ToolStep['chunks']): { text: string; complete: boolean } | null {
  if (!chunks) return null;
  let text = '';
  let complete = true;
  // `total` is at most MAX_TOOL_CHUNKS; the loop also ends once the cap is filled.
  for (let i = 0; i < chunks.total && text.length <= MAX_TOOL_TEXT; i++) {
    const slice = chunks.received[i];
    if (slice === undefined) complete = false;
    else text += slice;
  }
  return { text, complete };
}

/** Folds one tool frame into the step list (keyed by `tool_run_id`). */
export function reduceToolFrame(steps: readonly ToolStep[], node: NodeEvent): ToolStep[] {
  const rm = asRecord(node.response_metadata) ?? {};

  if (node.type === 'agent_tool_output_chunk') {
    const chunk = asRecord(rm.tool_output_chunk);
    const id = asString(chunk?.tool_call_id);
    const index = asCount(chunk?.index);
    const total = asCount(chunk?.total);
    const slice = typeof node.content === 'string' ? node.content : null;
    if (id === null || index === null || total === null || total === 0 || index >= total || slice === null) {
      return [...steps];
    }
    const existing = steps.find((s) => s.id === id) ?? { ...emptyStep(id, 'tool'), partial: true };
    // A total the app will not loop over or index: the output is shown as partial.
    if (total > MAX_TOOL_CHUNKS) return upsert(steps, { ...existing, partial: true, chunks: null });
    const chunks = existing.chunks && existing.chunks.total === total ? existing.chunks : { total, received: {}, kept: 0 };
    // Bounded: once the cap is filled the rest is not recorded (the assembled
    // output is already over it and reads as truncated), and a repeated index
    // changes nothing.
    if (chunks.kept > MAX_TOOL_TEXT || chunks.received[index] !== undefined) return [...steps];
    const kept = slice.slice(0, MAX_TOOL_TEXT + 1);
    const received = { ...chunks.received, [index]: kept };
    return upsert(steps, { ...existing, chunks: { total, received, kept: chunks.kept + kept.length } });
  }

  const id = asString(rm.tool_run_id) ?? asString(rm.run_id);
  if (id === null) return [...steps];
  const meta = toolMeta(rm);
  const found = steps.find((s) => s.id === id);
  let step: ToolStep = found ?? { ...emptyStep(id, meta.name), partial: node.type !== 'agent_tool_start' };
  step = {
    ...step,
    name: meta.name !== 'tool' ? meta.name : step.name,
    displayName: meta.displayName ?? step.displayName,
    toolkit: meta.toolkit ?? asName(rm.toolkit_name) ?? step.toolkit,
    parentAgent: meta.parentAgent ?? step.parentAgent,
    startedAt: step.startedAt ?? parseTime(rm.timestamp_start),
  };
  step = withInputs(step, rm.tool_inputs);

  switch (node.type) {
    case 'agent_tool_start':
      return upsert(steps, step);
    case 'agent_tool_end': {
      const declared = asRecord(rm.tool_output_chunks);
      const declaredTotal = asCount(declared?.total);
      let output = displayValue(rm.tool_output);
      let partial = step.partial;
      if ((output === null || output === '') && declaredTotal !== null && declaredTotal > MAX_TOOL_CHUNKS) {
        output = '';
        partial = true;
      } else if ((output === null || output === '') && declaredTotal !== null && declaredTotal > 0) {
        const chunks = step.chunks && step.chunks.total === declaredTotal ? step.chunks : { total: declaredTotal, received: {}, kept: 0 };
        const joined = assembled(chunks);
        output = joined?.text ?? '';
        partial = partial || !joined?.complete;
      }
      const capped = output === null ? null : capText(output);
      return upsert(steps, {
        ...step,
        status: 'done',
        endedAt: parseTime(rm.timestamp_finish),
        output: capped?.text ?? null,
        outputTruncated: capped?.truncated ?? false,
        partial,
      });
    }
    case 'agent_tool_error': {
      const error = displayValue(rm.error) ?? (typeof node.content === 'string' ? node.content : null);
      // The Python worker reports the approval pause of a sensitive tool as
      // an error of that call (the LangGraph interrupt's repr) just before the
      // agent_hitl_interrupt frame. The tool has not run, and the repr (which
      // carries the call's arguments) is not an error to show.
      if (error !== null && isInterruptRepr(error)) {
        return upsert(steps, { ...step, status: 'waiting', error: null });
      }
      const capped = error === null ? null : capText(error);
      return upsert(steps, {
        ...step,
        status: 'error',
        endedAt: parseTime(rm.timestamp_finish),
        error: capped?.text ?? 'The tool failed.',
      });
    }
    case 'mcp_authorization_required':
      // The tool has not run: it waits for a sign-in.
      return upsert(steps, { ...step, status: step.status === 'running' ? 'needs_auth' : step.status });
    default:
      return [...steps];
  }
}

/** A LangGraph interrupt's repr, as a tool error: `(Interrupt(value={...}, id='...'),)`, possibly JSON-quoted. */
export function isInterruptRepr(text: string): boolean {
  return /^\W{0,4}Interrupt\(value=/.test(text.trimStart());
}

function upsert(steps: readonly ToolStep[], step: ToolStep): ToolStep[] {
  const index = steps.findIndex((s) => s.id === step.id);
  if (index < 0) return [...steps, step];
  const next = [...steps];
  next[index] = step;
  return next;
}

export function isToolFrame(type: string): boolean {
  return (
    type === 'agent_tool_start' ||
    type === 'agent_tool_end' ||
    type === 'agent_tool_error' ||
    type === 'agent_tool_output_chunk'
  );
}

// ---------------------------------------------------------------------------
// Pauses

/** One pending human-in-the-loop interrupt. */
export interface HitlInterrupt {
  interruptId: string | null;
  toolCallId: string | null;
  message: string | null;
  availableActions: string[];
  guardrailType: string | null;
  actionLabel: string | null;
  policyMessage: string | null;
  toolName: string | null;
  toolkitName: string | null;
  /** The tool call's arguments (masked in the card until revealed). */
  toolArgs: Record<string, unknown> | null;
  /** ask_user questions, as sent (read loosely by the card). */
  questions: unknown[];
  parentAgentName: string | null;
  childThreadId: string | null;
}

/** One toolkit that needs a sign-in before its tool can run. */
export interface AuthRequest {
  /** What a continuation names (`authorization_request_id`). */
  requestId: string;
  toolCallId: string | null;
  serverUrl: string | null;
  toolName: string | null;
  toolkitName: string | null;
  toolkitType: string | null;
  resourceName: string | null;
}

export type TurnPause =
  | {
      kind: 'hitl';
      /** single: one interrupt; parallel: several in one terminal frame; fanout: child pauses (not terminal). */
      shape: 'single' | 'parallel' | 'fanout';
      interrupts: HitlInterrupt[];
      threadId: string | null;
      /** The paused answer's message id (the continuation's `message_id`). */
      messageId: string | null;
    }
  | { kind: 'auth'; requests: AuthRequest[]; threadId: string | null; messageId: string | null }
  | { kind: 'output_limit'; threadId: string | null; messageId: string | null; action: string | null };

const asStrings = (v: unknown): string[] =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string' && x !== '').slice(0, 16) : [];

export function parseInterrupt(raw: unknown, fallback: Record<string, unknown> = {}): HitlInterrupt | null {
  const r = asRecord(raw);
  if (!r) return null;
  const meta = asRecord(r.metadata) ?? asRecord(fallback.metadata) ?? {};
  const get = (key: string) => r[key] ?? fallback[key];
  const interruptId = asString(get('interrupt_id'));
  const toolCallId = asString(get('tool_call_id'));
  const actions = asStrings(get('available_actions'));
  if (interruptId === null && actions.length === 0) return null;
  return {
    interruptId,
    toolCallId,
    message: asString(get('message')),
    availableActions: actions,
    guardrailType: asString(get('guardrail_type')),
    actionLabel: asName(get('action_label')),
    policyMessage: asString(get('policy_message')),
    toolName: asName(get('tool_name')),
    toolkitName: asName(get('toolkit_name')),
    toolArgs: asRecord(get('tool_args')),
    questions: Array.isArray(get('questions')) ? (get('questions') as unknown[]).slice(0, 20) : [],
    parentAgentName: asName(r.parent_agent_name) ?? asName(meta.parent_agent_name),
    childThreadId: asString(r.child_thread_id) ?? asString(meta.child_thread_id),
  };
}

/** The interrupts of an `agent_hitl_interrupt` frame (or a persisted group's metadata). */
export function interruptsOf(rm: Record<string, unknown>): HitlInterrupt[] {
  const many = Array.isArray(rm.hitl_interrupts) ? rm.hitl_interrupts : [];
  const out: HitlInterrupt[] = [];
  for (const raw of many) {
    const parsed = parseInterrupt(raw);
    if (parsed && !out.some((i) => i.interruptId !== null && i.interruptId === parsed.interruptId)) out.push(parsed);
  }
  if (out.length === 0) {
    // A single pause: in `hitl_interrupt`, or flat at the top of the metadata.
    const single = parseInterrupt(rm.hitl_interrupt ?? rm, rm);
    if (single) out.push(single);
  }
  return out;
}

export function isFanoutChild(rm: Record<string, unknown>): boolean {
  const meta = asRecord(rm.metadata) ?? {};
  return Boolean(asString(meta.parent_agent_name)) && Boolean(asString(meta.child_thread_id));
}

export function parseAuthRequest(raw: unknown): AuthRequest | null {
  const r = asRecord(raw);
  if (!r) return null;
  const requestId =
    asString(r.authorization_request_id) ?? asString(r.interrupt_id) ?? asString(r.tool_run_id) ?? asString(r.tool_call_id);
  if (requestId === null) return null;
  const resource = asRecord(r.resource_metadata);
  return {
    requestId,
    toolCallId: asString(r.tool_call_id),
    serverUrl: asString(r.server_url),
    toolName: asName(r.tool_name),
    toolkitName: asName(r.toolkit_name),
    toolkitType: asName(r.toolkit_type),
    resourceName: asName(resource?.resource_name),
  };
}

/** The pause one frame announces (null for every other frame, and for auth progress). */
export function pauseOf(node: NodeEvent): TurnPause | null {
  const rm = asRecord(node.response_metadata) ?? {};
  const threadId = asString(rm.thread_id) ?? asString(rm.root_thread_id);
  const messageId = asString(node.message_id);
  switch (node.type) {
    case 'agent_hitl_interrupt': {
      const interrupts = interruptsOf(rm);
      if (interrupts.length === 0) return null;
      const shape = isFanoutChild(rm) ? 'fanout' : interrupts.length > 1 ? 'parallel' : 'single';
      if (shape === 'fanout') {
        const meta = asRecord(rm.metadata) ?? {};
        for (const i of interrupts) {
          i.parentAgentName ??= asName(meta.parent_agent_name);
          i.childThreadId ??= asString(meta.child_thread_id);
        }
      }
      return { kind: 'hitl', shape, interrupts, threadId, messageId };
    }
    case 'mcp_authorization_required': {
      if (!Array.isArray(rm.authorization_requests)) return null;
      const requests = rm.authorization_requests.map(parseAuthRequest).filter((r): r is AuthRequest => r !== null);
      return requests.length > 0 ? { kind: 'auth', requests, threadId, messageId } : null;
    }
    case 'agent_requires_confirmation':
      return {
        kind: 'output_limit',
        threadId,
        messageId,
        action: typeof node.content === 'string' && node.content.trim() !== '' ? node.content.trim().slice(0, 80) : null,
      };
    default:
      return null;
  }
}

/** Adds a fan-out child's interrupts to the pending ones (one card each). */
export function mergeFanout(current: TurnPause | null, next: TurnPause): TurnPause {
  if (next.kind !== 'hitl' || next.shape !== 'fanout' || current?.kind !== 'hitl' || current.shape !== 'fanout') return next;
  const interrupts = [...current.interrupts];
  for (const i of next.interrupts) {
    if (!interrupts.some((x) => x.interruptId !== null && x.interruptId === i.interruptId)) interrupts.push(i);
  }
  return { ...current, interrupts, threadId: current.threadId ?? next.threadId, messageId: current.messageId ?? next.messageId };
}

/**
 * The persisted calls of an answer that is paused for approval. The server
 * stores the paused call as an error (the Python worker reports the
 * interrupt as a tool error), so the call the pause waits on would read as
 * failed under the very card that asks to approve it. The latest failed
 * call of each tool a pending interrupt names is shown as waiting instead.
 */
export function markPausedSteps<T extends ToolStep>(steps: readonly T[], pause: TurnPause | null): readonly T[] {
  if (pause?.kind !== 'hitl') return steps;
  const names = new Set(pause.interrupts.map((i) => i.toolName).filter((n): n is string => n !== null));
  if (names.size === 0) return steps;
  const next = [...steps];
  for (const name of names) {
    for (let i = next.length - 1; i >= 0; i--) {
      if (next[i].name !== name) continue;
      if (next[i].status === 'error') next[i] = { ...next[i], status: 'waiting', error: null };
      break;
    }
  }
  return next;
}

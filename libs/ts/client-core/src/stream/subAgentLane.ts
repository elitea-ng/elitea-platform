/**
 * The cloud sub-agent lane (client contract 1.48, ADR-0050 decision 4).
 *
 * A cloud run that starts temporary sub-agents (`spawn_agent`) streams each
 * child's lifecycle as `agent_subagent` frames: `response_metadata.subagent_v1`
 * is one entry (`started`, `resumed`, `tool_start`, `tool_end`, `text`,
 * `finished`). Main folds the same entries onto the answer's
 * `metadata.subagents_v1`, so a reload reads the settled lanes. This module
 * folds the live entries and reads the stored lanes into ONE shape, with
 * the same rules and bounds as Main's fold
 * (services/elitea-main/internal/infra/db/repos/subagent_lane_projection.go),
 * so the live lane and the reloaded lane agree.
 *
 * Every text here is untrusted model or tool output: render it as plain text.
 */

/** A lane's status (`ChildStatus` of the runtime). */
export type SubAgentLaneStatus = 'running' | 'completed' | 'failed' | 'budget_exhausted' | 'cancelled';

/** One of a child's own tool calls, or one of its texts. */
export type SubAgentLaneItem =
  | {
      readonly type: 'tool';
      readonly callId: string;
      readonly tool: string;
      readonly argsSummary?: string;
      /** Set once the call returned. */
      readonly ok?: boolean;
      readonly summary?: string;
    }
  | { readonly type: 'text'; readonly text: string };

/** One temporary sub-agent of a cloud answer. */
export interface CloudSubAgentLane {
  readonly agentId: string;
  /** The `tool_run_id` of the `spawn_agent` call that started it. */
  readonly parentCallId: string;
  readonly ordinal: number;
  readonly label?: string;
  readonly displayName?: string;
  /** The profile's avatar key (`default:<name>`). */
  readonly avatar?: string;
  readonly status: SubAgentLaneStatus;
  readonly items: readonly SubAgentLaneItem[];
  readonly steps?: number;
  readonly inputTokens?: number;
  readonly outputTokens?: number;
  readonly result?: string;
  readonly error?: string;
}

export const MAX_SUBAGENT_LANES = 24;
export const MAX_SUBAGENT_LANE_ITEMS = 96;
const MAX_ID_CHARS = 128;
const MAX_NAME_CHARS = 120;
const MAX_SHORT_CHARS = 240;
const MAX_TEXT_CHARS = 600;
const MAX_RESULT_CHARS = 2000;

const STATUSES: ReadonlySet<string> = new Set(['running', 'completed', 'failed', 'budget_exhausted', 'cancelled']);

type Raw = Record<string, unknown>;

const isObject = (value: unknown): value is Raw => typeof value === 'object' && value !== null && !Array.isArray(value);

/** `value` without control characters other than newline and tab, cut to `max` characters. */
function text(value: unknown, max: number): string | undefined {
  if (typeof value !== 'string') return undefined;
  let out = '';
  let count = 0;
  for (const char of value) {
    const code = char.codePointAt(0) ?? 0;
    if ((code < 0x20 && char !== '\n' && char !== '\t') || code === 0x7f) continue;
    if (count === max) break;
    out += char;
    count += 1;
  }
  return out === '' ? undefined : out;
}

function id(value: unknown): string | undefined {
  if (typeof value !== 'string' || value === '' || value.length > MAX_ID_CHARS || /[\0\r\n]/.test(value)) return undefined;
  return value;
}

function count(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : undefined;
}

/**
 * Fold one `subagent_v1` entry into the lanes. Returns the same array when
 * the entry does not apply (unknown kind, unknown child, over a bound), so a
 * caller can skip a re-render by identity.
 */
export function foldSubAgentEntry(lanes: readonly CloudSubAgentLane[], entry: unknown): readonly CloudSubAgentLane[] {
  if (!isObject(entry)) return lanes;
  const agentId = id(entry.agent_id);
  if (agentId === undefined) return lanes;
  const index = lanes.findIndex((lane) => lane.agentId === agentId);
  if (entry.kind === 'started') {
    const parentCallId = id(entry.parent_call_id);
    const ordinal = count(entry.ordinal);
    if (parentCallId === undefined || index !== -1 || lanes.length >= MAX_SUBAGENT_LANES || !ordinal || ordinal > MAX_SUBAGENT_LANES) {
      return lanes;
    }
    const lane: CloudSubAgentLane = {
      agentId,
      parentCallId,
      ordinal,
      status: 'running',
      items: [],
      ...optional('label', text(entry.label, MAX_NAME_CHARS)),
      ...optional('displayName', text(entry.display_name, MAX_NAME_CHARS)),
      ...optional('avatar', text(entry.avatar, MAX_NAME_CHARS)),
    };
    return [...lanes, lane];
  }
  if (index === -1) return lanes;
  const lane = lanes[index];
  if (lane === undefined) return lanes;
  const next = foldInto(lane, entry);
  if (next === lane) return lanes;
  const out = [...lanes];
  out[index] = next;
  return out;
}

function foldInto(lane: CloudSubAgentLane, entry: Raw): CloudSubAgentLane {
  switch (entry.kind) {
    case 'resumed': {
      const { result: _result, error: _error, ...rest } = lane;
      return { ...rest, status: 'running' };
    }
    case 'tool_start': {
      const callId = id(entry.call_id);
      if (callId === undefined || lane.items.length >= MAX_SUBAGENT_LANE_ITEMS) return lane;
      if (lane.items.some((item) => item.type === 'tool' && item.callId === callId)) return lane;
      const item: SubAgentLaneItem = {
        type: 'tool',
        callId,
        tool: text(entry.tool, MAX_NAME_CHARS) ?? '',
        ...optional('argsSummary', text(entry.args_summary, MAX_SHORT_CHARS)),
      };
      return { ...lane, items: [...lane.items, item] };
    }
    case 'tool_end': {
      const callId = id(entry.call_id);
      if (callId === undefined || typeof entry.ok !== 'boolean') return lane;
      const ok = entry.ok;
      const summary = text(entry.summary, MAX_SHORT_CHARS);
      let found = false;
      const items = lane.items.map((item) => {
        if (item.type !== 'tool' || item.callId !== callId) return item;
        found = true;
        const { summary: _old, ...rest } = item;
        return { ...rest, ok, ...optional('summary', summary) };
      });
      return found ? { ...lane, items } : lane;
    }
    case 'text': {
      const value = text(entry.text, MAX_TEXT_CHARS);
      if (value === undefined || value.trim() === '' || lane.items.length >= MAX_SUBAGENT_LANE_ITEMS) return lane;
      return { ...lane, items: [...lane.items, { type: 'text', text: value }] };
    }
    case 'finished': {
      if (typeof entry.status !== 'string' || !STATUSES.has(entry.status)) return lane;
      const { result: _result, error: _error, ...rest } = lane;
      return {
        ...rest,
        status: entry.status as SubAgentLaneStatus,
        ...optional('steps', count(entry.steps) ?? lane.steps),
        ...optional('inputTokens', count(entry.input_tokens) ?? lane.inputTokens),
        ...optional('outputTokens', count(entry.output_tokens) ?? lane.outputTokens),
        ...optional('result', text(entry.result, MAX_RESULT_CHARS)),
        ...optional('error', text(entry.error, MAX_NAME_CHARS)),
      };
    }
    default:
      return lane;
  }
}

function optional<K extends string, V>(key: K, value: V | undefined): { [P in K]?: V } {
  return (value === undefined ? {} : { [key]: value }) as { [P in K]?: V };
}

/** The `subagent_v1` entry of an `agent_subagent` frame, if it carries one. */
export function subAgentEntryOf(frame: unknown): unknown {
  if (!isObject(frame) || !isObject(frame.response_metadata)) return undefined;
  return frame.response_metadata.subagent_v1;
}

/**
 * The lanes a stored answer keeps (`metadata.subagents_v1`). Anything that
 * is not a lane list reads as none; a malformed lane or item is skipped.
 */
export function parseSubAgentLanes(value: unknown): readonly CloudSubAgentLane[] {
  if (!Array.isArray(value)) return [];
  const lanes: CloudSubAgentLane[] = [];
  for (const raw of value.slice(0, MAX_SUBAGENT_LANES)) {
    if (!isObject(raw)) continue;
    const agentId = id(raw.agent_id);
    const parentCallId = id(raw.parent_call_id);
    const ordinal = count(raw.ordinal);
    if (agentId === undefined || parentCallId === undefined || !ordinal) continue;
    if (lanes.some((lane) => lane.agentId === agentId)) continue;
    const status = typeof raw.status === 'string' && STATUSES.has(raw.status) ? (raw.status as SubAgentLaneStatus) : 'cancelled';
    const items: SubAgentLaneItem[] = [];
    for (const item of Array.isArray(raw.items) ? raw.items.slice(0, MAX_SUBAGENT_LANE_ITEMS) : []) {
      if (!isObject(item)) continue;
      if (item.type === 'tool') {
        const callId = id(item.call_id);
        if (callId === undefined) continue;
        items.push({
          type: 'tool',
          callId,
          tool: text(item.tool, MAX_NAME_CHARS) ?? '',
          ...optional('argsSummary', text(item.args_summary, MAX_SHORT_CHARS)),
          ...optional('ok', typeof item.ok === 'boolean' ? item.ok : undefined),
          ...optional('summary', text(item.summary, MAX_SHORT_CHARS)),
        });
      } else if (item.type === 'text') {
        const value = text(item.text, MAX_TEXT_CHARS);
        if (value !== undefined) items.push({ type: 'text', text: value });
      }
    }
    lanes.push({
      agentId,
      parentCallId,
      ordinal,
      status,
      items,
      ...optional('label', text(raw.label, MAX_NAME_CHARS)),
      ...optional('displayName', text(raw.display_name, MAX_NAME_CHARS)),
      ...optional('avatar', text(raw.avatar, MAX_NAME_CHARS)),
      ...optional('steps', count(raw.steps)),
      ...optional('inputTokens', count(raw.input_tokens)),
      ...optional('outputTokens', count(raw.output_tokens)),
      ...optional('result', text(raw.result, MAX_RESULT_CHARS)),
      ...optional('error', text(raw.error, MAX_NAME_CHARS)),
    });
  }
  return lanes;
}

/**
 * A settled answer's lanes: a lane still `running` when its answer ended was
 * stopped with the run (a run that ended without the worker's last frame,
 * e.g. a crash, leaves one unended).
 */
export function settleSubAgentLanes(lanes: readonly CloudSubAgentLane[]): readonly CloudSubAgentLane[] {
  if (!lanes.some((lane) => lane.status === 'running')) return lanes;
  return lanes.map((lane) => (lane.status === 'running' ? { ...lane, status: 'cancelled' } : lane));
}

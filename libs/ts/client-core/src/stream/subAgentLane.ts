/**
 * The cloud sub-agent lane (client contract 1.48, ADR-0050 decision 4;
 * nesting 1.50, #1872).
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
 * At `subagents.max_depth` 2 a `started` entry may carry `depth` 2 and
 * `parent_agent_id`: the id of the sub-agent that started it. Both
 * `foldSubAgentEntry` (live) and `parseSubAgentLanes` (reload) nest such a
 * lane into its named parent's own `children` rather than the top level, so
 * `CloudSubAgentLane[]` returned to a caller is always the run's direct
 * (depth-1) children, each carrying its own settled grandchildren.
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
  /**
   * Client contract 1.50 (#1872): 1 for a child of the run's agent, 2 for a
   * child of a child (agents.subagents_max_depth). Absent reads as 1 (an
   * older answer).
   */
  readonly depth?: number;
  /**
   * Client contract 1.50. The `agentId` of the sub-agent that started this
   * one; present only at depth 2. A depth-2 lane is folded under its
   * parent's `children` rather than kept at the top level.
   */
  readonly parentAgentId?: string;
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
  /**
   * Client contract 1.50 (#1872): this lane's own sub-agents (depth 2).
   * Never itself nested (sub-agent depth is at most 2).
   */
  readonly children?: readonly CloudSubAgentLane[];
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

/** Every agent id in `lanes`, at every depth (top-level and nested). */
function countLanes(lanes: readonly CloudSubAgentLane[]): number {
  let total = 0;
  for (const lane of lanes) total += 1 + countLanes(lane.children ?? []);
  return total;
}

function findLane(lanes: readonly CloudSubAgentLane[], agentId: string): CloudSubAgentLane | undefined {
  for (const lane of lanes) {
    if (lane.agentId === agentId) return lane;
    const found = findLane(lane.children ?? [], agentId);
    if (found !== undefined) return found;
  }
  return undefined;
}

/**
 * Replace the lane named `agentId`, wherever it sits (top-level or nested
 * under a parent's `children`, #1872), with `change`'s result. Returns
 * `lanes` unchanged (by identity) when `agentId` is not found or `change`
 * leaves its lane unchanged.
 */
function updateLane(lanes: readonly CloudSubAgentLane[], agentId: string, change: (lane: CloudSubAgentLane) => CloudSubAgentLane): readonly CloudSubAgentLane[] {
  let changed = false;
  const next = lanes.map((lane) => {
    if (lane.agentId === agentId) {
      const updated = change(lane);
      if (updated !== lane) changed = true;
      return updated;
    }
    if (lane.children === undefined) return lane;
    const children = updateLane(lane.children, agentId, change);
    if (children === lane.children) return lane;
    changed = true;
    return { ...lane, children };
  });
  return changed ? next : lanes;
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
  if (entry.kind === 'started') {
    const parentCallId = id(entry.parent_call_id);
    const ordinal = count(entry.ordinal);
    if (parentCallId === undefined || findLane(lanes, agentId) !== undefined || countLanes(lanes) >= MAX_SUBAGENT_LANES || !ordinal || ordinal > MAX_SUBAGENT_LANES) {
      return lanes;
    }
    // Client contract 1.50 (#1872): 1 or 2; absent or out of range reads as
    // 1 (a run's direct child). At depth 2 the entry must name a parent
    // already in `lanes`, or it is kept at the top level rather than
    // dropped (the same rule the server-side fold uses).
    const rawDepth = count(entry.depth);
    const parentAgentId = id(entry.parent_agent_id);
    // Only a top-level depth-1 lane can parent (never a nested lane, never
    // a depth-2 lane kept at the top level): nesting is one level deep.
    const parent = rawDepth === 2 && parentAgentId !== undefined ? lanes.find((l) => l.agentId === parentAgentId && l.depth === 1) : undefined;
    const depth = parent === undefined ? 1 : 2;
    const lane: CloudSubAgentLane = {
      agentId,
      parentCallId,
      ordinal,
      depth,
      status: 'running',
      items: [],
      ...(parent !== undefined ? { parentAgentId: parent.agentId } : {}),
      ...optional('label', text(entry.label, MAX_NAME_CHARS)),
      ...optional('displayName', text(entry.display_name, MAX_NAME_CHARS)),
      ...optional('avatar', text(entry.avatar, MAX_NAME_CHARS)),
    };
    if (parent === undefined) return [...lanes, lane];
    return updateLane(lanes, parent.agentId, (found) => ({ ...found, children: [...(found.children ?? []), lane] }));
  }
  if (findLane(lanes, agentId) === undefined) return lanes;
  return updateLane(lanes, agentId, (lane) => foldInto(lane, entry));
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
 * One lane of a stored `SubAgentLane[]` (`metadata.subagents_v1` or a
 * lane's own `children`, client contract 1.50, #1872): `depth` and
 * `parentAgentId` are carried through as-is (Main's fold already nested a
 * depth-2 lane under its parent; this reader does not re-nest), and
 * `children` is parsed the same way, recursively (at most one level in
 * practice — sub-agent depth is at most 2 — but a malformed extra level is
 * still read rather than refused). `undefined` for anything that is not an
 * object with the required fields.
 */
function parseOneSubAgentLane(raw: unknown, seen: ReadonlySet<string>, nestable = true): CloudSubAgentLane | undefined {
  if (!isObject(raw)) return undefined;
  const agentId = id(raw.agent_id);
  const parentCallId = id(raw.parent_call_id);
  const ordinal = count(raw.ordinal);
  if (agentId === undefined || parentCallId === undefined || !ordinal || seen.has(agentId)) return undefined;
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
  const depth = count(raw.depth) === 2 ? 2 : 1;
  const parentAgentId = depth === 2 ? id(raw.parent_agent_id) : undefined;
  // One level of nesting: only a depth-1 lane carries children, and its
  // children carry none (the same rule the server-side fold applies).
  const children = nestable && depth === 1 ? parseSubAgentLanes(raw.children, new Set([...seen, agentId]), false) : [];
  return {
    agentId,
    parentCallId,
    ordinal,
    depth,
    items,
    status,
    ...optional('parentAgentId', parentAgentId),
    ...optional('label', text(raw.label, MAX_NAME_CHARS)),
    ...optional('displayName', text(raw.display_name, MAX_NAME_CHARS)),
    ...optional('avatar', text(raw.avatar, MAX_NAME_CHARS)),
    ...optional('steps', count(raw.steps)),
    ...optional('inputTokens', count(raw.input_tokens)),
    ...optional('outputTokens', count(raw.output_tokens)),
    ...optional('result', text(raw.result, MAX_RESULT_CHARS)),
    ...optional('error', text(raw.error, MAX_NAME_CHARS)),
    ...(children.length > 0 ? { children } : {}),
  };
}

/**
 * The lanes a stored answer keeps (`metadata.subagents_v1`). Anything that
 * is not a lane list reads as none; a malformed lane or item is skipped.
 * `seen` guards every level against an agent id reused at an outer level
 * (never written by Main, but a reader must not loop on it).
 */
export function parseSubAgentLanes(value: unknown, seen: ReadonlySet<string> = new Set(), nestable = true): readonly CloudSubAgentLane[] {
  if (!Array.isArray(value)) return [];
  const lanes: CloudSubAgentLane[] = [];
  const ids = new Set(seen);
  for (const raw of value.slice(0, MAX_SUBAGENT_LANES)) {
    const lane = parseOneSubAgentLane(raw, ids, nestable);
    if (lane === undefined) continue;
    ids.add(lane.agentId);
    lanes.push(lane);
  }
  return lanes;
}

/**
 * A settled answer's lanes: a lane still `running` when its answer ended was
 * stopped with the run (a run that ended without the worker's last frame,
 * e.g. a crash, leaves one unended). #1872: a nested (depth-2) lane settles
 * the same way, so a parent that ended before a grandchild's own end frame
 * does not leave the grandchild showing `running` forever.
 */
export function settleSubAgentLanes(lanes: readonly CloudSubAgentLane[]): readonly CloudSubAgentLane[] {
  const settleOne = (lane: CloudSubAgentLane): CloudSubAgentLane => {
    const children = lane.children === undefined ? undefined : settleSubAgentLanes(lane.children);
    if (lane.status !== 'running' && children === lane.children) return lane;
    return {
      ...lane,
      ...(lane.status === 'running' ? { status: 'cancelled' } : {}),
      ...(children === undefined ? {} : { children }),
    };
  };
  const next = lanes.map(settleOne);
  return next.every((lane, index) => lane === lanes[index]) ? lanes : next;
}

/**
 * The agent's rich meta blocks the editor controls (issue 3137): the
 * heartbeat schedule (`meta.proactivity`), memory settings (`meta.memory`),
 * the tool envelope (`meta.tool_envelope`), capabilities
 * (`meta.capabilities`) and run caps (`meta.run_caps`), each held as the wire
 * object (`null` for none) and saved into the version's meta by the editor's
 * one Save, like the soul.
 *
 * The controls write raw input: a number field that does not hold a number
 * keeps its text, so `metaBlockProblems` can name it and Save waits. The
 * limits are v2.yaml's (AgentMemorySettings, AgentProactivity,
 * AgentToolEnvelope, VersionRunCaps); a run cap is also held under the
 * platform's cap for the agent's risk tier (getAgentRunCaps), which the save
 * refuses above with RUN_CAP_ABOVE_TIER.
 */
import { t } from '@/shared/i18n';

const META_BLOCK_KEYS = ['memory', 'proactivity', 'tool_envelope', 'capabilities', 'run_caps'] as const;
export type MetaBlockKey = (typeof META_BLOCK_KEYS)[number];
export type MetaBlock = Readonly<Record<string, unknown>>;
export type MetaBlocks = Readonly<Record<MetaBlockKey, MetaBlock | null>>;

/** The platform's caps at the agent's tier; 0 is no cap. */
export type TierRunCaps = Readonly<Partial<Record<RunCapKey, number>>>;
export const RUN_CAP_KEYS = ['max_tool_calls', 'max_model_turns', 'max_records_read', 'max_spend'] as const;
export type RunCapKey = (typeof RUN_CAP_KEYS)[number];

type Rec = Record<string, unknown>;

function asRec(value: unknown): Rec | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Rec) : null;
}

export function readMetaBlocks(meta: Readonly<Rec> | undefined): MetaBlocks {
  return Object.fromEntries(META_BLOCK_KEYS.map((key) => [key, asRec(meta?.[key])])) as MetaBlocks;
}

/** Key-order-insensitive JSON, so a re-seeded block equal by value is not "dirty". */
function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(',')}]`;
  const record = asRec(value);
  if (record === null) return JSON.stringify(value) ?? 'undefined';
  return `{${Object.keys(record)
    .sort()
    .map((key) => `${JSON.stringify(key)}:${stable(record[key])}`)
    .join(',')}}`;
}

export function areMetaBlocksEqual(a: MetaBlocks, b: MetaBlocks): boolean {
  return META_BLOCK_KEYS.every((key) => stable(a[key]) === stable(b[key]));
}

/**
 * The blocks as the meta body sends them: a block that is set is sent; one
 * cleared that the stored meta had is sent as `null` (every block is nullable
 * on the wire), so the merge over the stored meta cannot keep it.
 */
export function metaBlocksWriteField(stored: Readonly<Rec>, blocks: MetaBlocks): Rec {
  const out: Rec = {};
  for (const key of META_BLOCK_KEYS) {
    if (blocks[key] !== null) out[key] = blocks[key];
    else if (key in stored) out[key] = null;
  }
  return out;
}

/** `block` with `path` set to `value`, or removed for `undefined`; an emptied block becomes `null`. */
export function withMetaValue(block: MetaBlock | null, path: readonly string[], value: unknown): MetaBlock | null {
  const [head, ...rest] = path;
  if (head === undefined) return block;
  const next: Rec = { ...block };
  const child = rest.length === 0 ? value : (withMetaValue(asRec(next[head]), rest, value) ?? undefined);
  if (child === undefined) delete next[head];
  else next[head] = child;
  return Object.keys(next).length === 0 ? null : next;
}

/** A number field's value: a number when it reads as one, `undefined` when empty, else the raw text (refused). */
export function numberInput(text: string): number | string | undefined {
  const trimmed = text.trim();
  if (trimmed === '') return undefined;
  const value = Number(trimmed);
  return Number.isFinite(value) ? value : text;
}

/* ── validation ─────────────────────────────────────────────────────────── */

export type MetaProblems = ReadonlyMap<string, string>;

interface Bound {
  readonly min: number;
  readonly max: number;
  readonly integer: boolean;
  readonly decimals?: number;
}

function boundProblem(value: unknown, bound: Bound): string | undefined {
  if (value === undefined || value === null) return undefined;
  const ok =
    typeof value === 'number' &&
    value >= bound.min &&
    value <= bound.max &&
    (!bound.integer || Number.isInteger(value)) &&
    (bound.decimals === undefined || Math.round(value * 10 ** bound.decimals) === value * 10 ** bound.decimals);
  if (ok) return undefined;
  return bound.integer
    ? t('pages.agents.editApplication.meta.wholeNumberBetween', 'A whole number from {{min}} to {{max}}.', { min: bound.min, max: bound.max })
    : t('pages.agents.editApplication.meta.numberBetween', 'A number from {{min}} to {{max}}.', { min: bound.min, max: bound.max });
}

function check(problems: Map<string, string>, path: string, value: unknown, bound: Bound): void {
  const problem = boundProblem(value, bound);
  if (problem !== undefined) problems.set(path, problem);
}

function memoryProblems(block: MetaBlock | null, problems: Map<string, string>): void {
  if (block === null) return;
  const recall = asRec(block['recall']) ?? {};
  check(problems, 'memory.recall.max_entries', recall['max_entries'], { min: 1, max: 50, integer: true });
  check(problems, 'memory.recall.max_tokens', recall['max_tokens'], { min: 100, max: 8000, integer: true });
}

const DURATION = /^(?:(\d+)h)?(?:(\d+)m)?$/;

/** A Go duration of whole minutes ("1h", "90m", "1h30m"), in minutes; `undefined` when it is not one. */
function durationMinutes(text: string): number | undefined {
  const match = DURATION.exec(text.trim());
  if (match === null || text.trim() === '') return undefined;
  return Number(match[1] ?? 0) * 60 + Number(match[2] ?? 0);
}

function isTimeZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat('en', { timeZone: zone });
    return true;
  } catch {
    // Handled (§3.6): an unknown zone is a refusal, not a crash.
    return false;
  }
}

const QUIET_HOURS = /^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d (\S+)$/;

/** `"HH:MM-HH:MM Zone"` with an IANA zone. */
function isQuietHours(text: string): boolean {
  const match = QUIET_HOURS.exec(text);
  return match !== null && isTimeZone(match[3] ?? '');
}

function scheduleProblems(block: MetaBlock, problems: Map<string, string>): void {
  const interval = block['interval'];
  if (interval !== undefined) {
    const minutes = typeof interval === 'string' ? durationMinutes(interval) : undefined;
    if (minutes === undefined || minutes < 15 || minutes > 24 * 60) {
      problems.set('proactivity.interval', t('pages.agents.editApplication.meta.interval', 'From 15m to 24h, in whole minutes ("30m", "1h", "1h30m").'));
    }
  }
  check(problems, 'proactivity.jitter', block['jitter'], { min: 0, max: 50, integer: true });
  const quiet = block['quiet_hours'];
  if (quiet !== undefined && (typeof quiet !== 'string' || !isQuietHours(quiet))) {
    problems.set('proactivity.quiet_hours', t('pages.agents.editApplication.meta.quietHours', 'A start and an end as HH:MM, and a time zone such as Europe/Berlin.'));
  }
}

function proactivityProblems(block: MetaBlock | null, problems: Map<string, string>): void {
  if (block === null) return;
  scheduleProblems(block, problems);
  const watch = Array.isArray(block['watch']) ? (block['watch'] as Rec[]) : [];
  if (watch.some((item) => item['kind'] === 'toolkit_read' && (typeof item['tool'] !== 'string' || item['tool'] === '' || item['tool'].length > 128))) {
    problems.set('proactivity.watch', t('pages.agents.editApplication.meta.watchTool', 'Name the read-only tool to watch, at most 128 characters.'));
  }
  const limits = asRec(block['limits']) ?? {};
  check(problems, 'proactivity.limits.wakes_per_day', limits['wakes_per_day'], { min: 1, max: 96, integer: true });
  check(problems, 'proactivity.limits.acts_per_day', limits['acts_per_day'], { min: 0, max: 96, integer: true });
  check(problems, 'proactivity.limits.spend_per_day_usd', limits['spend_per_day_usd'], { min: 0, max: 1000, integer: false });
}

function envelopeProblems(block: MetaBlock | null, problems: Map<string, string>): void {
  if (block === null) return;
  const limits = Object.entries(asRec(block['tool_limits']) ?? {});
  if (limits.length > 256 || limits.some(([key, value]) => key.trim() === '' || boundProblem(value, { min: 1, max: 10000, integer: true }) !== undefined)) {
    problems.set('tool_envelope.tool_limits', t('pages.agents.editApplication.meta.toolLimits', 'One "toolkit/tool = limit" per line, each limit a whole number from 1 to 10000.'));
  }
  for (const key of ['unattended_tools', 'subagent_tools']) {
    const list = block[key];
    if (Array.isArray(list) && list.length > 256) problems.set(`tool_envelope.${key}`, t('pages.agents.editApplication.meta.tooManyTools', 'At most 256 tools.'));
  }
}

const RUN_CAP_BOUNDS: Readonly<Record<RunCapKey, Bound>> = {
  max_tool_calls: { min: 1, max: 100000, integer: true },
  max_model_turns: { min: 1, max: 100000, integer: true },
  max_records_read: { min: 1, max: 1000000000, integer: true },
  max_spend: { min: 0.0001, max: 1000000, integer: false, decimals: 4 },
};

function runCapProblems(block: MetaBlock | null, tierCaps: TierRunCaps, problems: Map<string, string>): void {
  if (block === null) return;
  for (const key of RUN_CAP_KEYS) {
    const value = block[key];
    const path = `run_caps.${key}`;
    check(problems, path, value, RUN_CAP_BOUNDS[key]);
    const cap = tierCaps[key];
    if (!problems.has(path) && typeof value === 'number' && cap !== undefined && cap > 0 && value > cap) {
      problems.set(path, t('pages.agents.editApplication.meta.aboveTierCap', 'At most {{cap}}, the platform cap for this agent\'s risk tier.', { cap }));
    }
  }
}

/** Every value the controls hold that Main would refuse, by its path under meta. Empty is saveable. */
export function metaBlockProblems(blocks: MetaBlocks, tierCaps: TierRunCaps = {}): MetaProblems {
  const problems = new Map<string, string>();
  memoryProblems(blocks.memory, problems);
  proactivityProblems(blocks.proactivity, problems);
  envelopeProblems(blocks.tool_envelope, problems);
  runCapProblems(blocks.run_caps, tierCaps, problems);
  return problems;
}

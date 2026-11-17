/**
 * The trajectory of a run's cases (issue 2011, step B): the steps and tool calls
 * of each case, built from the per-step run evidence the orchestrator copied
 * into the result evidence (`evidence.trajectory`) when the case ran as a real
 * agent run (execution mode `runtime_plane`).
 *
 * ABSENCE IS NOT ZERO. A case with no trajectory in its evidence (a
 * predict_blocking run, or a case whose agent run could not start) is not "0
 * steps": it is left out of the figures, and the card says how many cases it
 * measured. Nothing here is computed for a run that has no trajectory at all.
 */
import type { EvalRunResult } from '@/shared/api/generated/model';

import { percentile } from './runInsights';

export interface TrajectoryItem {
  readonly seq: number;
  readonly kind: string;
  readonly name: string;
  readonly toolkitId: string | undefined;
  readonly durationMs: number | undefined;
  readonly error: string | undefined;
  readonly output: string | undefined;
}

interface ToolFigures {
  readonly name: string;
  readonly toolkitId: string | undefined;
  readonly calls: number;
  readonly errors: number;
}

/**
 * What the guardrails did to one case's run (`evidence.guardrail`, issue 2192):
 * LLM gateway verdicts that blocked, masked or warned, and governance gates
 * (sensitive-tool approval requests). Absent when the verdicts were not read.
 */
export interface CaseGuardrail {
  readonly events: number;
  readonly blocked: number;
  readonly masked: number;
  readonly warned: number;
  readonly gated: number;
}

export interface CaseTrajectory {
  readonly caseId: string;
  /** What the case asked, for a row label. */
  readonly input: string | undefined;
  readonly steps: number;
  readonly modelCalls: number;
  readonly toolCalls: number;
  readonly toolErrors: number;
  readonly retries: number;
  /** Unique tools the case used; `undefined` for a case stored before issue 2192. */
  readonly distinctTools: number | undefined;
  /** Repeats of an identical, already successful call; `undefined` as above. */
  readonly redundantCalls: number | undefined;
  readonly guardrail: CaseGuardrail | undefined;
  readonly tools: readonly ToolFigures[];
  readonly items: readonly TrajectoryItem[];
  /** True when the step list was cut; the counts still cover every step. */
  readonly truncated: boolean;
  readonly timedOut: boolean;
  readonly stepLimit: boolean;
}

export interface TrajectorySummary {
  readonly cases: readonly CaseTrajectory[];
  /** Mean of the distinct tools per case, over the cases that recorded it. */
  readonly distinctToolsPerCase: number | undefined;
  /** Unique tools used by any case of the run. */
  readonly distinctToolsInRun: number | undefined;
  readonly redundantCalls: number | undefined;
  /** Redundant calls over the tool calls of the cases that recorded them (0..1). */
  readonly redundantShare: number | undefined;
  /** Guardrail events over the cases whose verdicts were read; `undefined` when none were. */
  readonly guardrailEvents: number | undefined;
  readonly guardrailCases: number;
  /** Cases that ended at the step limit, of every case of the run. */
  readonly stepLimitCases: number;
  readonly stepLimitCaseIds: readonly string[];
  readonly totalCases: number;
  readonly stepsP50: number | undefined;
  readonly stepsP95: number | undefined;
  readonly toolCalls: number;
  readonly toolErrors: number;
  readonly retries: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function count(record: Record<string, unknown>, key: string): number | undefined {
  const value = record[key];
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined;
}

function text(record: Record<string, unknown>, key: string): string | undefined {
  const value = record[key];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function parseGuardrail(raw: unknown): CaseGuardrail | undefined {
  if (!isRecord(raw)) return undefined;
  const events = count(raw, 'events');
  if (events === undefined) return undefined;
  return {
    events,
    blocked: count(raw, 'blocked') ?? 0,
    masked: count(raw, 'masked') ?? 0,
    warned: count(raw, 'warned') ?? 0,
    gated: count(raw, 'gated') ?? 0,
  };
}

function parseItem(raw: unknown): TrajectoryItem | undefined {
  if (!isRecord(raw)) return undefined;
  return {
    seq: count(raw, 'seq') ?? 0,
    kind: text(raw, 'kind') ?? '',
    name: text(raw, 'name') ?? '',
    toolkitId: text(raw, 'toolkit_id'),
    durationMs: count(raw, 'duration_ms'),
    error: text(raw, 'error'),
    output: text(raw, 'output'),
  };
}

function parseTool(raw: unknown): ToolFigures | undefined {
  if (!isRecord(raw)) return undefined;
  return {
    name: text(raw, 'name') ?? '',
    toolkitId: text(raw, 'toolkit_id'),
    calls: count(raw, 'calls') ?? 0,
    errors: count(raw, 'errors') ?? 0,
  };
}

function list<T>(raw: unknown, parse: (entry: unknown) => T | undefined): T[] {
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((entry) => {
    const parsed = parse(entry);
    return parsed === undefined ? [] : [parsed];
  });
}

type CaseCounts = Pick<CaseTrajectory, 'modelCalls' | 'toolCalls' | 'toolErrors' | 'retries' | 'distinctTools' | 'redundantCalls'>;

function parseCounts(raw: Record<string, unknown>): CaseCounts {
  return {
    modelCalls: count(raw, 'model_calls') ?? 0,
    toolCalls: count(raw, 'tool_calls') ?? 0,
    toolErrors: count(raw, 'tool_errors') ?? 0,
    retries: count(raw, 'retries') ?? 0,
    distinctTools: count(raw, 'distinct_tools'),
    redundantCalls: count(raw, 'redundant_calls'),
  };
}

/** One case's trajectory from a result's evidence, or `undefined` when it has none. */
function parseCase(result: EvalRunResult): CaseTrajectory | undefined {
  const evidence: Record<string, unknown> | undefined = result.evidence;
  const raw = evidence?.['trajectory'];
  if (!isRecord(raw)) return undefined;
  const steps = count(raw, 'steps');
  if (steps === undefined) return undefined;
  const input = evidence?.['input'];
  return {
    caseId: result.dataset_case_id,
    input: typeof input === 'string' ? input : undefined,
    steps,
    ...parseCounts(raw),
    guardrail: parseGuardrail(evidence?.['guardrail']),
    tools: list(raw['tools'], parseTool),
    items: list(raw['items'], parseItem),
    truncated: raw['truncated'] === true,
    timedOut: evidence?.['agent_timed_out'] === true,
    stepLimit: evidence?.['agent_step_limit'] === true,
  };
}

function sumOf(values: readonly (number | undefined)[]): number | undefined {
  const known = values.filter((value): value is number => value !== undefined);
  return known.length === 0 ? undefined : known.reduce((total, value) => total + value, 0);
}

function round1(value: number): number {
  return Math.round(value * 10) / 10;
}

function distinctToolsFigures(measured: readonly CaseTrajectory[]): { perCase: number | undefined; inRun: number | undefined } {
  const withTools = measured.filter((entry) => entry.distinctTools !== undefined);
  if (withTools.length === 0) return { perCase: undefined, inRun: undefined };
  const total = sumOf(withTools.map((entry) => entry.distinctTools)) ?? 0;
  const union = new Set(withTools.flatMap((entry) => entry.tools.map((tool) => `${tool.toolkitId ?? ''}\u0000${tool.name}`)));
  return { perCase: round1(total / withTools.length), inRun: union.size };
}

function redundantFigures(measured: readonly CaseTrajectory[]): { count: number | undefined; share: number | undefined } {
  const withFigure = measured.filter((entry) => entry.redundantCalls !== undefined);
  const redundant = sumOf(withFigure.map((entry) => entry.redundantCalls));
  const calls = withFigure.reduce((total, entry) => total + entry.toolCalls, 0);
  return { count: redundant, share: redundant === undefined || calls === 0 ? undefined : redundant / calls };
}

interface SeenCases {
  readonly caseIds: Set<string>;
  readonly cases: Map<string, CaseTrajectory>;
  readonly stepLimit: Set<string>;
  readonly guardrails: Map<string, CaseGuardrail>;
}

/** Collapses the rows of each case: its facts are on every row, its trajectory on one. */
function collectCases(results: readonly EvalRunResult[]): SeenCases {
  const seen: SeenCases = { caseIds: new Set(), cases: new Map(), stepLimit: new Set(), guardrails: new Map() };
  for (const result of results) {
    const id = result.dataset_case_id;
    seen.caseIds.add(id);
    const evidence: Record<string, unknown> | undefined = result.evidence;
    if (evidence?.['agent_step_limit'] === true) seen.stepLimit.add(id);
    const guardrail = seen.guardrails.has(id) ? undefined : parseGuardrail(evidence?.['guardrail']);
    if (guardrail !== undefined) seen.guardrails.set(id, guardrail);
    const parsed = seen.cases.has(id) ? undefined : parseCase(result);
    if (parsed !== undefined) seen.cases.set(id, parsed);
  }
  return seen;
}

/**
 * The trajectories of a run's cases, one per case (its facts are stored on every
 * result row of the case, so rows collapse by case id), or `undefined` when no
 * case has one. The step-limit flag and the guardrail figures are read from
 * every case, whether or not it recorded steps: a run blocked at its first
 * request has no steps and still has a guardrail event.
 */
export function summarizeTrajectory(results: readonly EvalRunResult[]): TrajectorySummary | undefined {
  const { caseIds, cases, stepLimit, guardrails } = collectCases(results);
  if (cases.size === 0 && stepLimit.size === 0 && guardrails.size === 0) return undefined;
  const measured = [...cases.values()];
  const steps = measured.map((entry) => entry.steps);
  const tools = distinctToolsFigures(measured);
  const redundant = redundantFigures(measured);
  return {
    cases: measured,
    totalCases: caseIds.size,
    stepsP50: percentile(steps, 0.5),
    stepsP95: percentile(steps, 0.95),
    toolCalls: measured.reduce((total, entry) => total + entry.toolCalls, 0),
    toolErrors: measured.reduce((total, entry) => total + entry.toolErrors, 0),
    retries: measured.reduce((total, entry) => total + entry.retries, 0),
    distinctToolsPerCase: tools.perCase,
    distinctToolsInRun: tools.inRun,
    redundantCalls: redundant.count,
    redundantShare: redundant.share,
    guardrailEvents: sumOf([...guardrails.values()].map((entry) => entry.events)),
    guardrailCases: guardrails.size,
    stepLimitCases: stepLimit.size,
    stepLimitCaseIds: [...stepLimit],
  };
}

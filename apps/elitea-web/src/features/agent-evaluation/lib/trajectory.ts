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

export interface CaseTrajectory {
  readonly caseId: string;
  /** What the case asked, for a row label. */
  readonly input: string | undefined;
  readonly steps: number;
  readonly modelCalls: number;
  readonly toolCalls: number;
  readonly toolErrors: number;
  readonly retries: number;
  readonly tools: readonly ToolFigures[];
  readonly items: readonly TrajectoryItem[];
  /** True when the step list was cut; the counts still cover every step. */
  readonly truncated: boolean;
  readonly timedOut: boolean;
  readonly stepLimit: boolean;
}

export interface TrajectorySummary {
  readonly cases: readonly CaseTrajectory[];
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
    modelCalls: count(raw, 'model_calls') ?? 0,
    toolCalls: count(raw, 'tool_calls') ?? 0,
    toolErrors: count(raw, 'tool_errors') ?? 0,
    retries: count(raw, 'retries') ?? 0,
    tools: list(raw['tools'], parseTool),
    items: list(raw['items'], parseItem),
    truncated: raw['truncated'] === true,
    timedOut: evidence?.['agent_timed_out'] === true,
    stepLimit: evidence?.['agent_step_limit'] === true,
  };
}

/**
 * The trajectories of a run's cases, one per case (its facts are stored on every
 * result row of the case, so rows collapse by case id), or `undefined` when no
 * case has one.
 */
export function summarizeTrajectory(results: readonly EvalRunResult[]): TrajectorySummary | undefined {
  const caseIds = new Set<string>();
  const cases = new Map<string, CaseTrajectory>();
  for (const result of results) {
    caseIds.add(result.dataset_case_id);
    if (cases.has(result.dataset_case_id)) continue;
    const parsed = parseCase(result);
    if (parsed !== undefined) cases.set(result.dataset_case_id, parsed);
  }
  if (cases.size === 0) return undefined;
  const measured = [...cases.values()];
  const steps = measured.map((entry) => entry.steps);
  return {
    cases: measured,
    totalCases: caseIds.size,
    stepsP50: percentile(steps, 0.5),
    stepsP95: percentile(steps, 0.95),
    toolCalls: measured.reduce((total, entry) => total + entry.toolCalls, 0),
    toolErrors: measured.reduce((total, entry) => total + entry.toolErrors, 0),
    retries: measured.reduce((total, entry) => total + entry.retries, 0),
  };
}

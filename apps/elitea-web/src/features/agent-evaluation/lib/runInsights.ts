/**
 * The arithmetic behind a run's insights (issue 2011, step A): per-case
 * latency, the efficiency strip, the consumption summary and the comparison
 * with the previous run.
 *
 * ABSENCE IS NOT ZERO. Every figure here is `undefined` when its input is
 * missing: a run scored before the release that records latency has no
 * `agent_latency_ms`, and reporting a p50 of 0 ms for it would claim the agent
 * answered instantly. Callers render `undefined` as "not recorded".
 */
import type { AnalyticsEvaluationRun, EvalRun, EvalRunResult } from '@/shared/api/generated/model';

/** One case's agent turn, as the orchestrator recorded it in the result evidence. */
interface CaseTurn {
  readonly caseId: string;
  readonly latencyMs: number | undefined;
  readonly timedOut: boolean;
}

/**
 * One turn per case. The agent turn runs once per case and its facts are stored
 * on every result row of that case, so rows are collapsed by case id.
 */
export function caseTurns(results: readonly EvalRunResult[]): CaseTurn[] {
  const turns = new Map<string, CaseTurn>();
  for (const result of results) {
    if (turns.has(result.dataset_case_id)) continue;
    const evidence: Record<string, unknown> | undefined = result.evidence;
    const latency = evidence?.['agent_latency_ms'];
    turns.set(result.dataset_case_id, {
      caseId: result.dataset_case_id,
      latencyMs: typeof latency === 'number' && Number.isFinite(latency) ? latency : undefined,
      timedOut: evidence?.['agent_timed_out'] === true,
    });
  }
  return [...turns.values()];
}

/** Nearest-rank percentile of an unsorted list; `undefined` for an empty one. */
export function percentile(values: readonly number[], fraction: number): number | undefined {
  if (values.length === 0) return undefined;
  const sorted = [...values].sort((left, right) => left - right);
  const rank = Math.max(1, Math.ceil(fraction * sorted.length));
  return sorted[Math.min(rank, sorted.length) - 1];
}

export interface Efficiency {
  /** Cases that carry a recorded latency. */
  readonly measuredCases: number;
  readonly totalCases: number;
  readonly p50Ms: number | undefined;
  readonly p95Ms: number | undefined;
  readonly timeouts: number;
  readonly tokensPerCase: number | undefined;
  readonly costPerCase: number | undefined;
}

export function summarizeEfficiency(
  turns: readonly CaseTurn[],
  analytics: AnalyticsEvaluationRun | undefined,
): Efficiency {
  const latencies = turns.flatMap((turn) => (turn.latencyMs === undefined ? [] : [turn.latencyMs]));
  const cases = turns.length;
  const totals = analytics?.available === true ? analytics.totals : undefined;
  return {
    measuredCases: latencies.length,
    totalCases: cases,
    p50Ms: percentile(latencies, 0.5),
    p95Ms: percentile(latencies, 0.95),
    timeouts: turns.filter((turn) => turn.timedOut).length,
    tokensPerCase: totals !== undefined && cases > 0 ? Math.round(totals.total_tokens / cases) : undefined,
    costPerCase: totals?.total_cost !== undefined && cases > 0 ? totals.total_cost / cases : undefined,
  };
}

/** Average normalised score per dimension over the `ok` rows. */
export function dimensionAverages(results: readonly EvalRunResult[]): Map<string, number> {
  const sums = new Map<string, { total: number; count: number }>();
  for (const result of results) {
    if (result.status !== 'ok' || result.normalized_score === null || result.normalized_score === undefined) continue;
    const entry = sums.get(result.dimension_id) ?? { total: 0, count: 0 };
    entry.total += result.normalized_score;
    entry.count += 1;
    sums.set(result.dimension_id, entry);
  }
  return new Map([...sums].map(([id, { total, count }]) => [id, total / count]));
}

/** `current - previous`, or `undefined` when either side is missing. */
function delta(current: number | null | undefined, previous: number | null | undefined): number | undefined {
  if (current === null || current === undefined || previous === null || previous === undefined) return undefined;
  return current - previous;
}

interface DimensionDelta {
  readonly dimensionId: string;
  readonly current: number;
  readonly previous: number;
  readonly delta: number;
}

export interface RunComparison {
  readonly previousRunId: string;
  readonly headline: number | undefined;
  readonly dimensions: readonly DimensionDelta[];
  readonly p50Ms: number | undefined;
  readonly costPerCase: number | undefined;
}

interface Side {
  readonly run: EvalRun;
  readonly results: readonly EvalRunResult[];
  readonly efficiency: Efficiency;
}

/** Only dimensions BOTH runs scored are compared: a delta needs two numbers. */
export function compareRuns(current: Side, previous: Side): RunComparison {
  const now = dimensionAverages(current.results);
  const before = dimensionAverages(previous.results);
  const dimensions: DimensionDelta[] = [];
  for (const [dimensionId, value] of now) {
    const old = before.get(dimensionId);
    if (old !== undefined) dimensions.push({ dimensionId, current: value, previous: old, delta: value - old });
  }
  return {
    previousRunId: previous.run.id,
    headline: delta(current.run.headline_score, previous.run.headline_score),
    dimensions,
    p50Ms: delta(current.efficiency.p50Ms, previous.efficiency.p50Ms),
    costPerCase: delta(current.efficiency.costPerCase, previous.efficiency.costPerCase),
  };
}

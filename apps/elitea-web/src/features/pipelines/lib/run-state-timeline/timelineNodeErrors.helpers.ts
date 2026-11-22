/**
 * Structured `node_errors` extraction & continued-failure attribution for
 * grouped run timelines (5b-1 D12).
 *
 * Mirrors `ui/state/runStateValues.ts`'s `continuedFailureSteps` deduplication
 * and nearest-owner-at-or-before attribution rules while also exposing
 * structured fields (`supportRef`, `handled`, `inputStep`, `code`, `message`)
 * so UI components can display `"Failed · continued"` details safely via React
 * text nodes without ever constructing raw HTML.
 */
import { MAX_NODE_ERRORS_SCAN } from './timeline.constants';
import type { ContinuedFailureHandling, GroupedTimelineStep, TimelineContinuedFailure } from './timeline.types';

interface ParsedContinuedEntry {
  readonly failure: TimelineContinuedFailure;
  readonly identity: string;
}

function asNonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function asFiniteNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function parseContinuedHandling(value: unknown): ContinuedFailureHandling | undefined {
  return value === 'continue' || value === 'error_output' ? value : undefined;
}

function asRecord(value: unknown): Readonly<Record<string, unknown>> | undefined {
  return typeof value === 'object' && value !== null ? (value as Readonly<Record<string, unknown>>) : undefined;
}

function readErrorString(
  errorObj: Readonly<Record<string, unknown>> | undefined,
  record: Readonly<Record<string, unknown>>,
  key: string,
): string | undefined {
  return asNonEmptyString(errorObj?.[key]) ?? asNonEmptyString(record[key]);
}

function parseContinuedEntry(entry: unknown): ParsedContinuedEntry | undefined {
  const record = asRecord(entry);
  if (!record) return undefined;
  const node = asNonEmptyString(record['node']);
  const handled = parseContinuedHandling(record['handled']);
  if (!node || !handled) return undefined;

  const errorObj = asRecord(record['error']);
  const inputStep = asFiniteNumber(record['input_step']);
  const supportRef = readErrorString(errorObj, record, 'support_ref');
  const code = readErrorString(errorObj, record, 'code');
  const message = readErrorString(errorObj, record, 'message');

  const failure: TimelineContinuedFailure = {
    node,
    handled,
    ...(inputStep !== undefined ? { inputStep } : {}),
    ...(supportRef !== undefined ? { supportRef } : {}),
    ...(code !== undefined ? { code } : {}),
    ...(message !== undefined ? { message } : {}),
  };

  return {
    failure,
    identity: JSON.stringify([node, inputStep, supportRef, handled]),
  };
}

function nearestStepOf(steps: readonly GroupedTimelineStep[], node: string, from: number): number | undefined {
  for (let index = from; index >= 0; index -= 1) {
    const candidate = steps[index];
    if (candidate?.id === node || candidate?.langgraph_node === node) return index;
  }
  return undefined;
}

/**
 * Attributes each unique `node_errors` entry with `handled: 'continue' | 'error_output'`
 * to the nearest step at or before its first appearance whose `id` or `langgraph_node`
 * matches `entry.node`.
 */
export function extractContinuedFailuresByStep(
  steps: readonly GroupedTimelineStep[],
): ReadonlyMap<number, readonly TimelineContinuedFailure[]> {
  const byStep = new Map<number, TimelineContinuedFailure[]>();
  const seen = new Set<string>();

  steps.forEach((step, index) => {
    const rawEntries = step.state?.['node_errors'];
    if (!Array.isArray(rawEntries)) return;

    const limit = Math.min(rawEntries.length, MAX_NODE_ERRORS_SCAN);
    for (let entryIndex = 0; entryIndex < limit; entryIndex += 1) {
      const parsed = parseContinuedEntry(rawEntries[entryIndex]);
      if (!parsed || seen.has(parsed.identity)) continue;
      seen.add(parsed.identity);

      const owner = nearestStepOf(steps, parsed.failure.node, index);
      if (owner === undefined) continue;

      const list = byStep.get(owner);
      if (list) list.push(parsed.failure);
      else byStep.set(owner, [parsed.failure]);
    }
  });

  return byStep;
}

/** True when any step in `[start, end]` has an attributed continued failure. */
export function hasContinuedFailureInRange(
  continuedByStep: ReadonlyMap<number, readonly TimelineContinuedFailure[]>,
  start: number,
  end: number,
): boolean {
  for (let index = start; index <= end; index += 1) {
    if ((continuedByStep.get(index)?.length ?? 0) > 0) return true;
  }
  return false;
}

/** Collects all structured continued failures across `[start, end]` in step order. */
export function collectContinuedFailuresInRange(
  continuedByStep: ReadonlyMap<number, readonly TimelineContinuedFailure[]>,
  start: number,
  end: number,
): readonly TimelineContinuedFailure[] {
  const result: TimelineContinuedFailure[] = [];
  for (let index = start; index <= end; index += 1) {
    const entries = continuedByStep.get(index);
    if (entries) result.push(...entries);
  }
  return result;
}

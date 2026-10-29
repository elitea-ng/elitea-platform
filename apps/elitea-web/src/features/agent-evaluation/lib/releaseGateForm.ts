import type { ReleaseGateSuite, ReleaseGateSuiteRequest } from '@/shared/api/generated/model';

/**
 * The release gate form of one dataset, and its conversion to the PUT body.
 *
 * The server's rules (internal/releasegate/criteria.go), checked here so the
 * Save button is off rather than the save refused: a gate suite has 1 to 5
 * criteria, at most one per dimension; a `threshold` criterion has a
 * `min_score` and a `no_worse_than` criterion a `max_drop`, both 0-100; the
 * re-evaluation cadence is 1-365 days or off. Numbers are strings while
 * typed, converted once.
 */
export const MAX_GATE_CRITERIA = 5;

export type CriterionKind = 'threshold' | 'no_worse_than';

export interface CriterionDraft {
  readonly dimensionId: string;
  readonly kind: CriterionKind;
  readonly value: string;
}

export interface ReleaseGateDraft {
  readonly releaseGate: boolean;
  readonly regression: boolean;
  readonly criteria: readonly CriterionDraft[];
  readonly reevaluateEveryDays: string;
}

export function draftFromSuite(suite: ReleaseGateSuite | undefined): ReleaseGateDraft {
  if (suite === undefined || !suite.release_gate) {
    return { releaseGate: false, regression: false, criteria: [], reevaluateEveryDays: '' };
  }
  return {
    releaseGate: true,
    regression: suite.regression ?? false,
    criteria: (suite.criteria ?? []).map((criterion) => ({
      dimensionId: criterion.dimension_id,
      kind: criterion.kind,
      value: String((criterion.kind === 'threshold' ? criterion.min_score : criterion.max_drop) ?? ''),
    })),
    reevaluateEveryDays:
      suite.reevaluate_every_days === null || suite.reevaluate_every_days === undefined
        ? ''
        : String(suite.reevaluate_every_days),
  };
}

function scoreValid(value: string): boolean {
  const number = Number(value);
  return value.trim() !== '' && Number.isFinite(number) && number >= 0 && number <= 100;
}

function cadenceValid(value: string): boolean {
  if (value.trim() === '') return true;
  const number = Number(value);
  return Number.isInteger(number) && number >= 1 && number <= 365;
}

/** Why the draft cannot be saved, or undefined when it can. */
export function draftProblem(draft: ReleaseGateDraft): 'criteria' | 'duplicate' | 'value' | 'cadence' | undefined {
  if (!draft.releaseGate) return undefined;
  if (draft.criteria.length === 0 || draft.criteria.length > MAX_GATE_CRITERIA) return 'criteria';
  const dimensions = draft.criteria.map((criterion) => criterion.dimensionId);
  if (dimensions.some((id) => id === '') || new Set(dimensions).size !== dimensions.length) return 'duplicate';
  if (!draft.criteria.every((criterion) => scoreValid(criterion.value))) return 'value';
  if (!cadenceValid(draft.reevaluateEveryDays)) return 'cadence';
  return undefined;
}

export function requestFromDraft(draft: ReleaseGateDraft): ReleaseGateSuiteRequest {
  if (!draft.releaseGate) return { release_gate: false };
  return {
    release_gate: true,
    regression: draft.regression,
    criteria: draft.criteria.map((criterion) =>
      criterion.kind === 'threshold'
        ? { dimension_id: criterion.dimensionId, kind: 'threshold', min_score: Number(criterion.value) }
        : { dimension_id: criterion.dimensionId, kind: 'no_worse_than', max_drop: Number(criterion.value) },
    ),
    reevaluate_every_days: draft.reevaluateEveryDays.trim() === '' ? null : Number(draft.reevaluateEveryDays),
  };
}

/**
 * The calls behind a run's insights and its Enhance with AI wizard, over the
 * GENERATED client, with the transport envelope unwrapped in one place (see
 * `evaluationRunsApi.ts` for why).
 *
 * ENHANCE WRITES NOTHING ITSELF. `enhanceRun` answers a proposal. Each accepted
 * fix is saved by `applyEnhancement` through the route that owns that write: a
 * new agent version, a dimension update, a dataset case update. So every
 * permission, and the agent release path, stay where they are.
 */
import { getEvaluationRunAnalytics } from '@/shared/api/generated/analytics/analytics';
import {
  enhanceEvalRun,
  getApplicationVersionDetail,
  listEvalRuns,
  saveApplicationNewVersion,
  updateEvalDatasetCase,
} from '@/shared/api/generated/applications/applications';
import type {
  AnalyticsEvaluationRun,
  ApplicationVersionDetail,
  EvalEnhanceProposal,
  EvalRun,
  SaveApplicationNewVersionBody,
} from '@/shared/api/generated/model';

import { dimensionWriteInput, caseWriteInput, draftVersionBody, draftVersionName } from '../lib/enhanceApply';
import type { EvalDimension } from '../model/types';
import { updateEvalDimension } from './evaluationApi';
import { fetchEvalDataset } from './evaluationRunsApi';

function body<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

export async function fetchRunAnalytics(projectId: string, runId: string): Promise<AnalyticsEvaluationRun> {
  return body<AnalyticsEvaluationRun>(await getEvaluationRunAnalytics(projectId, runId));
}

/** Rows asked per page while looking for the previous run, and how many pages are read. */
const PREVIOUS_PAGE_SIZE = 200;
const PREVIOUS_MAX_PAGES = 5;

/**
 * The nearest earlier scored run of the same suite (dataset) that ran in the
 * SAME execution mode. Modes are not comparable: a one-call-per-case run and an
 * agent run measure different things, so a delta across them is a false
 * regression or improvement.
 *
 * The agent VERSION is deliberately NOT matched: comparing a new version
 * against the one before it is the point of the comparison. The card names the
 * run and its version so the reader sees what the delta is against.
 *
 * The server filters by suite, agent and "scored" (`before_run_id`,
 * `scored_only`) and not by mode, so the mode is matched here, page by page,
 * walking back through older runs until one fits or the bounded history (1000
 * runs) is read. A server-side mode filter would remove the walk; it needs an
 * API contract change, so it is left as the follow-up.
 */
export async function fetchPreviousScoredRun(projectId: string, run: EvalRun): Promise<EvalRun | undefined> {
  const base = {
    dataset_id: run.dataset_id,
    scored_only: true,
    limit: PREVIOUS_PAGE_SIZE,
    ...(run.application_id === null || run.application_id === undefined ? {} : { application_id: run.application_id }),
  };
  let before = run.id;
  for (let page = 0; page < PREVIOUS_MAX_PAGES; page += 1) {
    const { rows } = body<{ readonly rows: EvalRun[] }>(await listEvalRuns(projectId, { ...base, before_run_id: before }));
    const match = rows.find((row) => row.execution_mode === run.execution_mode);
    if (match !== undefined) return match;
    const last = rows.at(-1);
    if (last === undefined || rows.length < PREVIOUS_PAGE_SIZE) return undefined;
    before = last.id;
  }
  return undefined;
}

export async function requestEnhancement(projectId: string, runId: string): Promise<EvalEnhanceProposal> {
  return body<EvalEnhanceProposal>(await enhanceEvalRun(projectId, runId));
}

/** What the reviewer accepted. */
export interface Acceptance {
  readonly instructions: boolean;
  readonly dimensionIds: ReadonlySet<string>;
  readonly caseIds: ReadonlySet<string>;
}

type AppliedKind = 'instructions' | 'dimension' | 'case';

/** One accepted fix and what happened to it. `error` is set when the write was refused. */
export interface AppliedFix {
  readonly kind: AppliedKind;
  /** The id the fix is keyed by: `instructions`, a dimension id or a case id. */
  readonly id: string;
  readonly label: string;
  readonly error?: unknown;
  /** The draft version an instruction patch created. */
  readonly version?: ApplicationVersionDetail;
}

async function attempt(
  kind: AppliedKind,
  id: string,
  label: string,
  write: () => Promise<ApplicationVersionDetail | undefined>,
): Promise<AppliedFix> {
  try {
    const version = await write();
    return version === undefined ? { kind, id, label } : { kind, id, label, version };
  } catch (error) {
    return { kind, id, label, error };
  }
}

async function applyInstructionPatch(
  projectId: string,
  proposal: EvalEnhanceProposal,
): Promise<ApplicationVersionDetail | undefined> {
  const patch = proposal.instruction_patch;
  if (patch === null || patch === undefined) return undefined;
  const applicationId = proposal.application_id;
  const versionId = proposal.application_version_id;
  if (applicationId === null || applicationId === undefined || versionId === null || versionId === undefined) {
    throw new Error('The run is not pinned to an agent version.');
  }
  // The PINNED version is the base, not the agent's current one: the patch was
  // written against what the run scored.
  const base = body<ApplicationVersionDetail>(await getApplicationVersionDetail(projectId, applicationId, versionId));
  const created = await saveApplicationNewVersion(projectId, applicationId, {
    ...draftVersionBody(base, patch.instructions),
    name: draftVersionName(proposal.run_id),
    copy_skills_from_version_id: versionId,
    copy_tools_from_version_id: versionId,
  } satisfies SaveApplicationNewVersionBody);
  return body<ApplicationVersionDetail>(created);
}

/**
 * Saves the accepted fixes one by one, in a fixed order, and reports each. A
 * refused write does not stop the others: they are independent, and the review
 * step shows which were saved and which were not.
 */
export async function applyEnhancement(
  projectId: string,
  proposal: EvalEnhanceProposal,
  accepted: Acceptance,
  library: readonly EvalDimension[],
): Promise<AppliedFix[]> {
  const applied: AppliedFix[] = [];
  if (accepted.instructions && proposal.instruction_patch !== null && proposal.instruction_patch !== undefined) {
    applied.push(await attempt('instructions', 'instructions', 'instructions', () => applyInstructionPatch(projectId, proposal)));
  }
  for (const change of proposal.dimension_changes) {
    if (!accepted.dimensionIds.has(change.dimension_id)) continue;
    applied.push(
      await attempt('dimension', change.dimension_id, change.dimension_name, async () => {
        const row = library.find((dimension) => dimension.id === change.dimension_id);
        if (row === undefined) throw new Error('The dimension is no longer in the library.');
        await updateEvalDimension(projectId, row.id, dimensionWriteInput(row, change));
        return undefined;
      }),
    );
  }
  for (const change of proposal.dataset_changes) {
    if (!accepted.caseIds.has(change.case_id)) continue;
    applied.push(
      await attempt('case', change.case_id, change.case_id, async () => {
        const dataset = await fetchEvalDataset(projectId, change.dataset_id);
        const row = dataset.cases.find((testCase) => testCase.id === change.case_id);
        if (row === undefined) throw new Error('The case is no longer in the dataset.');
        await updateEvalDatasetCase(projectId, change.dataset_id, change.case_id, caseWriteInput(row, change));
        return undefined;
      }),
    );
  }
  return applied;
}

/** The pinned version's instructions, so the wizard can show the patch against them. */
export async function fetchVersionInstructions(projectId: string, applicationId: number, versionId: number): Promise<string> {
  const detail = body<ApplicationVersionDetail>(await getApplicationVersionDetail(projectId, applicationId, versionId));
  return detail.instructions ?? '';
}

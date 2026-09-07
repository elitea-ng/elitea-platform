/**
 * The dataset and run calls, over the GENERATED client.
 *
 * WHY THESE GO THROUGH `shared/api/generated` AND THE DIMENSION FOUR DO NOT.
 *
 * `evaluationApi.ts` next to this file is hand-written, and its own header says
 * why: when it was authored the four dimension routes were not in
 * `api/openapi/v2.yaml`, and a manifest entry for an undescribed endpoint fails
 * elitea-main's `TestSpecRouterConformance/manifest_reverse_check`. The thirteen
 * operations here ARE described — slice 2 added them — so they have real
 * generated functions, real zod response schemas and real manifest entries.
 * The dimension four stay hand-written until somebody describes them; that is
 * an additive change to the spec and a separate one.
 *
 * WHAT THIS MODULE ADDS ON TOP OF THE GENERATED FUNCTIONS. One thing: it
 * unwraps the TRANSPORT ENVELOPE. `eliteaFetch` resolves
 * `{data, status, headers}`, and the generated response type says so — but the
 * shape is easy to type past, and a caller that treats the envelope as the body
 * reads `undefined` for every field on a perfectly good 200 (#132, which
 * shipped twice on one endpoint). One unwrap point, so no call site can make
 * that mistake.
 *
 * `body<T>()` is the same helper `features/agent-lifecycle/api/lifecycleApi.ts`
 * uses, and for the same reason: orval types `data` as the UNION of every
 * documented response body, success and error alike, because a `4xx` is a
 * legitimate shape of that operation. `eliteaFetch` THROWS an `EliteaApiError`
 * on any non-2xx, so a value that reaches a caller here has already been proved
 * to be the success branch — the union narrowing the compiler asks for would be
 * a check on a state the transport cannot deliver.
 */
import {
  addEvalDatasetCase,
  cancelEvalRun,
  createEvalDataset,
  deleteEvalDataset,
  deleteEvalDatasetCase,
  getEvalDataset,
  getEvalRun,
  getEvalScorecard,
  listEvalDatasets,
  listEvalRuns,
  startEvalRun,
  updateEvalDataset,
  updateEvalDatasetCase,
} from '@/shared/api/generated/applications/applications';
import type {
  EvalDataset,
  EvalDatasetCase,
  EvalDatasetCaseWriteRequest,
  EvalDatasetDetail,
  EvalDatasetWriteRequest,
  EvalRun,
  EvalRunStartRequest,
  EvalScorecard,
} from '@/shared/api/generated/model';

/**
 * The reference's own page size for a dataset's cases
 * (`EVAL_DATASET_CASE_PAGE_SIZE`). The server caps a dataset well below it, so
 * a page is never short in practice — the parameter is sent anyway, because the
 * cap is a product decision that can be raised and a read with no bound is the
 * thing that becomes a problem silently when it is.
 */
const DATASET_CASE_PAGE_SIZE = 200;

/** The server's own default for a scorecard page. */
const SCORECARD_PAGE_SIZE = 500;

/**
 * The success body of a generated response.
 *
 * `eliteaFetch` throws on any non-2xx (see `shared/api/generated/mutator.ts`),
 * so every value that reaches a caller of this module is the 2xx branch of the
 * union orval declares.
 */
function body<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

export async function fetchEvalDatasets(projectId: string, agentId?: number): Promise<EvalDataset[]> {
  const envelope = await listEvalDatasets(projectId, agentId === undefined ? undefined : { agent_id: agentId });
  // `.rows` and not `?? []`: the server always sends the key, and a silent
  // empty array here would turn a shape change into a blank page with nothing
  // in the console.
  return body<{ readonly rows: EvalDataset[] }>(envelope).rows;
}

export async function fetchEvalDataset(projectId: string, datasetId: string): Promise<EvalDatasetDetail> {
  const envelope = await getEvalDataset(projectId, datasetId, { limit: DATASET_CASE_PAGE_SIZE, offset: 0 });
  return body<EvalDatasetDetail>(envelope);
}

export async function createDataset(
  projectId: string,
  input: EvalDatasetWriteRequest,
): Promise<EvalDataset> {
  const envelope = await createEvalDataset(projectId, input);
  return body<EvalDataset>(envelope);
}

export async function renameDataset(
  projectId: string,
  datasetId: string,
  input: EvalDatasetWriteRequest,
): Promise<EvalDataset> {
  const envelope = await updateEvalDataset(projectId, datasetId, input);
  return body<EvalDataset>(envelope);
}

export async function removeDataset(projectId: string, datasetId: string): Promise<void> {
  await deleteEvalDataset(projectId, datasetId);
}

export async function addCase(
  projectId: string,
  datasetId: string,
  input: EvalDatasetCaseWriteRequest,
): Promise<EvalDatasetCase> {
  const envelope = await addEvalDatasetCase(projectId, datasetId, input);
  return body<EvalDatasetCase>(envelope);
}

export async function editCase(
  projectId: string,
  datasetId: string,
  caseId: string,
  input: EvalDatasetCaseWriteRequest,
): Promise<EvalDatasetCase> {
  const envelope = await updateEvalDatasetCase(projectId, datasetId, caseId, input);
  return body<EvalDatasetCase>(envelope);
}

export async function removeCase(projectId: string, datasetId: string, caseId: string): Promise<void> {
  await deleteEvalDatasetCase(projectId, datasetId, caseId);
}

export async function fetchEvalRuns(projectId: string, agentId?: number): Promise<EvalRun[]> {
  const envelope = await listEvalRuns(
    projectId,
    agentId === undefined ? undefined : { application_id: agentId },
  );
  return body<{ readonly rows: EvalRun[] }>(envelope).rows;
}

export async function fetchEvalRun(projectId: string, runId: string): Promise<EvalRun> {
  const envelope = await getEvalRun(projectId, runId);
  return body<EvalRun>(envelope);
}

export async function startRun(projectId: string, input: EvalRunStartRequest): Promise<EvalRun> {
  const envelope = await startEvalRun(projectId, input);
  return body<EvalRun>(envelope);
}

export async function cancelRun(projectId: string, runId: string): Promise<EvalRun> {
  const envelope = await cancelEvalRun(projectId, runId);
  return body<EvalRun>(envelope);
}

export async function fetchScorecard(projectId: string, runId: string): Promise<EvalScorecard> {
  const envelope = await getEvalScorecard(projectId, runId, { limit: SCORECARD_PAGE_SIZE, offset: 0 });
  return body<EvalScorecard>(envelope);
}

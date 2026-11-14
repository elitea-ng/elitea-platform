/**
 * The suite (run limits) and human score calls, over the GENERATED client.
 *
 * Kept apart from `evaluationRunsApi.ts` so a change to either family does not
 * touch the other. The same transport rule applies: `eliteaFetch` resolves
 * `{data, status, headers}` and throws on any non-2xx, so the body is read in
 * one place (`body`) and no call site can mistake the envelope for it.
 */
import {
  createEvalSuite,
  deleteEvalSuite,
  listEvalHumanScores,
  listEvalSuites,
  recordEvalHumanScore,
  updateEvalSuite,
} from '@/shared/api/generated/applications/applications';
import type {
  EvalHumanScore,
  EvalHumanScoreRecorded,
  EvalHumanScoreWriteRequest,
  EvalSuite,
  EvalSuiteWriteRequest,
} from '@/shared/api/generated/model';

function body<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

export async function fetchEvalSuites(projectId: string): Promise<EvalSuite[]> {
  const envelope = await listEvalSuites(projectId);
  return body<{ readonly rows: EvalSuite[] }>(envelope).rows;
}

export async function createSuite(projectId: string, input: EvalSuiteWriteRequest): Promise<EvalSuite> {
  return body<EvalSuite>(await createEvalSuite(projectId, input));
}

export async function saveSuite(
  projectId: string,
  suiteId: string,
  input: EvalSuiteWriteRequest,
): Promise<EvalSuite> {
  return body<EvalSuite>(await updateEvalSuite(projectId, suiteId, input));
}

export async function removeSuite(projectId: string, suiteId: string): Promise<void> {
  await deleteEvalSuite(projectId, suiteId);
}

export async function fetchHumanScores(projectId: string, runId: string): Promise<EvalHumanScore[]> {
  const envelope = await listEvalHumanScores(projectId, runId);
  return body<{ readonly rows: EvalHumanScore[] }>(envelope).rows;
}

export async function recordHumanScore(
  projectId: string,
  runId: string,
  input: EvalHumanScoreWriteRequest,
): Promise<EvalHumanScoreRecorded> {
  return body<EvalHumanScoreRecorded>(await recordEvalHumanScore(projectId, runId, input));
}

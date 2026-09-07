/**
 * The message to show a person when an evaluation write is refused.
 *
 * THE SERVER'S OWN TEXT WINS, AND READING IT TAKES A DELIBERATE STEP.
 * `eliteaFetch` throws an `EliteaApiError` whose `message` is a TRANSPORT
 * description — "eliteaFetch: 409 from …/eval_dataset_cases/…" — and the
 * response body is one level down, on `failure.body`. A handler that showed
 * `error.message` would look correct, compile, and throw away the only part
 * of the answer a person can act on. This slice's own UI test caught exactly
 * that before it shipped.
 *
 * The messages this recovers are not decoration. They are:
 *
 *   - "a dataset holds at most 10 cases: a run spends two model calls per
 *     case, so the cap is what stands between a large paste and an
 *     unauthorised bill" (409 on adding a case);
 *   - "this dataset has N run(s) scored against it: deleting it would destroy
 *     their results" (409 on deleting a dataset);
 *   - "dimension X cannot be scored by this release: it allows only the code
 *     engine, and this release serves the ai engine alone…" (501 on starting
 *     a run).
 *
 * Each names a limit, a count or a capability. "Failed to save" names none of
 * them, and a person who read it would try the same thing again.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

/** The `{error: "..."}` envelope every refusal in this API uses (pkg/apierr). */
function serverMessage(body: unknown): string | undefined {
  if (typeof body !== 'object' || body === null) return undefined;
  const message = (body as Record<string, unknown>)['error'];
  return typeof message === 'string' && message !== '' ? message : undefined;
}

function evaluationErrorMessage(error: unknown, fallback: string): string | undefined {
  if (error === null || error === undefined) return undefined;
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    return serverMessage(error.failure.body) ?? fallback;
  }
  // A network failure or an abort has no server body to recover, and its own
  // message ("network error — …") is the most specific thing there is.
  if (error instanceof Error && error.message !== '') return error.message;
  return fallback;
}

export function datasetErrorMessage(error: unknown): string | undefined {
  return evaluationErrorMessage(
    error,
    t('features.agentEvaluation.datasets.saveFailed', 'Failed to save the dataset.'),
  );
}

export function runErrorMessage(error: unknown): string | undefined {
  return evaluationErrorMessage(
    error,
    t('features.agentEvaluation.runs.startFailed', 'Failed to start the run.'),
  );
}

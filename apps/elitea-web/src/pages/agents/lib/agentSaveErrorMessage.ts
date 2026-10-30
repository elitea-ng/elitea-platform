import { applicationServerErrorMessage } from '@/features/agents';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The text of a save banner (the agent create and edit pages). A 422 is a
 * refusal the person can act on (OUTPUT_CONTRACT_INVALID, RUN_CAP_ABOVE_TIER,
 * …), so its `error` text shows, as the pipeline editor shows it. Any other
 * failure shows `fallback`: a 5xx body is a diagnostic, not advice.
 */
export function agentSaveErrorMessage(error: unknown, fallback: string): string {
  const isRefusal = error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 422;
  return isRefusal ? applicationServerErrorMessage(error, fallback) : fallback;
}

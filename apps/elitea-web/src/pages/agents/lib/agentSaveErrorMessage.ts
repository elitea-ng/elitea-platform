import { applicationServerErrorMessage } from '@/features/agents';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The text of a save banner (the agent create and edit pages). A 422 is a
 * refusal the person can act on (OUTPUT_CONTRACT_INVALID, RUN_CAP_ABOVE_TIER,
 * …), so its `error` text shows, as the pipeline editor shows it. So is a
 * 400 whose `code` is one of the soul refusals (ADR-0050: `soul_capability_key`,
 * `soul_too_large`, `soul_invalid`), which name the Persona field at fault.
 * Any other failure shows `fallback`: a 5xx body is a diagnostic, not advice.
 */
export function agentSaveErrorMessage(error: unknown, fallback: string): string {
  return isActionableRefusal(error) ? applicationServerErrorMessage(error, fallback) : fallback;
}

const SOUL_REFUSAL_CODES: ReadonlySet<string> = new Set(['soul_capability_key', 'soul_too_large', 'soul_invalid']);

function isActionableRefusal(error: unknown): boolean {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return false;
  if (error.failure.status === 422) return true;
  const body = error.failure.body;
  const code = typeof body === 'object' && body !== null ? (body as Record<string, unknown>)['code'] : undefined;
  return error.failure.status === 400 && typeof code === 'string' && SOUL_REFUSAL_CODES.has(code);
}

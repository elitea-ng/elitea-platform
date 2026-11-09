/**
 * The server's own refusal text, when it gave one: the proposal and copy
 * routes answer `{error, code}` for every refusal a person can act on
 * (`proposal_decided`, `proposal_decision_forbidden`, `copy_forbidden`), and
 * the create route its own `{error}`.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';

export function proposalErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const { body } = error.failure;
    if (typeof body === 'object' && body !== null) {
      const detail = (body as Record<string, unknown>)['error'];
      if (typeof detail === 'string' && detail !== '') return detail;
    }
    if (typeof body === 'string' && body !== '') return body;
  }
  return fallback;
}

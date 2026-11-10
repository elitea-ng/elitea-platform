/**
 * The server's own refusal text, when it gave one: the proposal and copy
 * routes answer `{error, code}` for every refusal a person can act on
 * (`proposal_decided`, `proposal_decision_forbidden`, `copy_forbidden`), and
 * the create route its own `{error}`.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The second step of a two-call write failed after the first made an agent
 * (Create: the create route, then the proposal's record; Copy to project:
 * the fork, then the memory copy). The agent exists: a retry must reuse
 * `applicationId` instead of making another one.
 */
export class AgentMadeError extends Error {
  readonly applicationId: number;
  override readonly cause: unknown;

  constructor(applicationId: number, cause: unknown) {
    super('the agent was made, the next step failed');
    this.name = 'AgentMadeError';
    this.applicationId = applicationId;
    this.cause = cause;
  }
}

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

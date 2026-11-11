/**
 * The proposal and copy routes answer `{error, code}` for every refusal a
 * person can act on (`proposal_decided`, `proposal_decision_forbidden`,
 * `copy_forbidden`), and the create route its own `{error}`; the shared
 * `serverErrorMessage` (`shared/api/serverErrorMessage.ts`) reads them.
 */

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


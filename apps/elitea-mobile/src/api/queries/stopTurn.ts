import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError } from '@/api/errors';

/**
 * Stop a running answer on the server (client contract 1.3,
 * cancelChatExecution: `DELETE /elitea_core/task/prompt_lib/{project_id}/{response_message_id}`).
 *
 * Closing the event stream alone does not stop the run: it keeps writing the
 * answer and spending the budget. The server answers 204 when it stopped the
 * run, and 204 again when the same caller already stopped it (also after the
 * run settled as cancelled); 409 when the answer is not running any more (it
 * completed or failed on its own) or is not this caller's to stop. After a
 * 204 the client closes the stream and reads the transcript again: partial
 * output is kept, a run stopped before any output is removed with its
 * question.
 */

export type StopOutcome =
  /** 204: stopped (or already stopped by this caller). */
  | 'stopped'
  /** 409: nothing to stop; the transcript holds whatever the answer became. */
  | 'not_running';

export type StopFailure = 'not_allowed' | 'not_available' | 'retry' | 'invalid' | 'failed';

export class StopError extends Error {
  constructor(readonly reason: StopFailure, readonly status?: number) {
    super(`stop: ${reason}`);
    this.name = 'StopError';
  }
}

export function stopErrorText(error: unknown): string {
  if (!(error instanceof StopError)) return "Couldn't stop the answer. Try again.";
  switch (error.reason) {
    case 'not_allowed':
      return "You can't stop this answer.";
    case 'not_available':
      return "This workspace can't stop answers from mobile yet.";
    case 'retry':
      return "Couldn't reach the workspace to stop the answer. Try again.";
    default:
      return "Couldn't stop the answer. Try again.";
  }
}

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

export async function stopTurn(
  projectId: number,
  responseMessageId: string,
  client: ApiClient = sessionApiClient(),
): Promise<StopOutcome> {
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !UUID.test(responseMessageId)) throw new StopError('invalid');
  let response: Response;
  try {
    ({ response } = await client.DELETE('/elitea_core/task/prompt_lib/{project_id}/{response_message_id}', {
      params: { path: { project_id: String(projectId), response_message_id: responseMessageId } },
    }));
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) throw new StopError('retry');
    throw error;
  }
  const status = response.status;
  if (status === 204 || status === 200) return 'stopped';
  if (status === 409) return 'not_running';
  if (status === 403) throw new StopError('not_allowed', status);
  if (status === 404 || status === 405 || status === 501) throw new StopError('not_available', status);
  if (status === 502 || status === 503 || status === 504) throw new StopError('retry', status);
  if (status === 400) throw new StopError('invalid', status);
  throw new StopError('failed', status);
}

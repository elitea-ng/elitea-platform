/**
 * api/contextViewApi.ts — "What the agent saw" (ADR-0050 decision 9, issue
 * 1669): the view Main recorded for one agent answer, through the generated
 * `getMessageContextView`.
 *
 *   GET /elitea_core/message_context_view/prompt_lib/{projectId}/{messageId}
 *
 * Every answer becomes a value, never a throw: a 404 is `not_recorded` (an
 * answer from before views were recorded, a reply that was not an agent's, or
 * another reader's view, which the server never serves), 401/403 are
 * `forbidden`, anything else `unavailable`. The ids are checked before they
 * reach the path.
 */
import { getMessageContextView } from '@/shared/api/generated/chat/chat';
import type { MessageContextView } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

const MESSAGE_ID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/u;
const PROJECT_ID = /^[1-9][0-9]{0,18}$/u;

export type ContextViewOutcome =
  | { readonly kind: 'view'; readonly view: MessageContextView }
  | { readonly kind: 'not_recorded' }
  | { readonly kind: 'forbidden' }
  | { readonly kind: 'unavailable' };

function outcomeOfError(error: unknown): ContextViewOutcome {
  if (!(error instanceof EliteaApiError)) return { kind: 'unavailable' };
  const failure = error.failure;
  if (failure.kind === 'auth') return { kind: 'forbidden' };
  if (failure.kind !== 'http') return { kind: 'unavailable' };
  if (failure.status === 404) return { kind: 'not_recorded' };
  if (failure.status === 401 || failure.status === 403) return { kind: 'forbidden' };
  return { kind: 'unavailable' };
}

/* manifest: chat.getMessageContextView */
export async function fetchMessageContextView(projectId: string, messageId: string): Promise<ContextViewOutcome> {
  if (!PROJECT_ID.test(projectId) || !MESSAGE_ID.test(messageId)) return { kind: 'not_recorded' };
  try {
    const envelope = await getMessageContextView(projectId, messageId);
    return { kind: 'view', view: (envelope as unknown as { readonly data: MessageContextView }).data };
  } catch (error) {
    return outcomeOfError(error);
  }
}

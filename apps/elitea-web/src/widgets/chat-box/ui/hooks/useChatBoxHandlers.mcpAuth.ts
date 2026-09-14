/** Use one authorization implementation for tokens, exact identities, and parallel decisions. */
import { resumeMcpAuthorization } from './useChatBoxHandlers.authorization';
import type { ChatBoxHandlerDeps, UseChatBoxHandlersResult } from './useChatBoxHandlers.helpers';

export function createResumeMcpFlow(
  deps: ChatBoxHandlerDeps,
): UseChatBoxHandlersResult['resumeMcpFlow'] {
  return (messageId, addToIgnoreList = false, authorizationRequestId) => resumeMcpAuthorization(
    deps,
    messageId,
    addToIgnoreList ? 'skip' : 'authorize',
    authorizationRequestId,
  );
}

import type { ConversationAction } from '@/api/queries/conversationActions';

/** The alert title when a Chats-list action did not go through (sent now and failed, or refused by the workspace). */
export function actionFailedTitle(action: ConversationAction): string {
  switch (action.kind) {
    case 'rename':
      return "Couldn't rename the chat";
    case 'pin':
      return "Couldn't change the pin";
    case 'archive':
      return action.archived ? "Couldn't archive the chat" : "Couldn't unarchive the chat";
    case 'delete':
      return "Couldn't delete the chat";
  }
}

/** The alert body for an action sent now that did not get an answer. */
export const ACTION_RETRY_BODY = 'Check your connection and try again.';
/** The alert body for a queued action the workspace refused (the list goes back to what it says). */
export const ACTION_REFUSED_BODY = 'The workspace did not accept the change.';

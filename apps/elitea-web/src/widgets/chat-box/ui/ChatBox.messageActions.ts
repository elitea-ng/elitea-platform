import type { ComponentProps } from 'react';

import type { ChatMessageList } from '@/features/chat-messages';

type ChatMessageListMessageActions = NonNullable<ComponentProps<typeof ChatMessageList>['messageActions']>;

/** `ChatMessageList`'s `messageActions`; a read-only conversation (`extensions.readOnlyNotice`) keeps copy and delete but cannot regenerate, edit-and-resend or fork (its fork could not be continued here either). Here rather than inline for `ChatBox`'s §3.5 complexity ceiling. */
export function buildMessageActions(readOnly: boolean, actions: {
  readonly onCopyToClipboard: ChatMessageListMessageActions['onCopyToClipboard'];
  readonly onDeleteAnswer: ChatMessageListMessageActions['onDeleteAnswer'];
  readonly onRegenerateAnswer: ChatMessageListMessageActions['onRegenerateAnswer'];
  readonly onSubmitEditedMessage: ChatMessageListMessageActions['onSubmitEditedMessage'];
  readonly onForkFromMessage?: ChatMessageListMessageActions['onForkFromMessage'];
}, noRegenerate = false): ChatMessageListMessageActions {
  if (readOnly) return { onCopyToClipboard: actions.onCopyToClipboard, onDeleteAnswer: actions.onDeleteAnswer };
  // Regenerate is dropped (edit-and-resend stays) while the newest answer ran on a desktop.
  if (noRegenerate) return { onCopyToClipboard: actions.onCopyToClipboard, onDeleteAnswer: actions.onDeleteAnswer, onSubmitEditedMessage: actions.onSubmitEditedMessage, onForkFromMessage: actions.onForkFromMessage };
  return actions;
}

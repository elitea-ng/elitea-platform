import type { ComponentProps } from 'react';

import type { ChatMessageList } from '@/features/chat-messages';

type ChatMessageListMessageActions = NonNullable<ComponentProps<typeof ChatMessageList>['messageActions']>;

/** `ChatMessageList`'s `messageActions`; a read-only conversation (`extensions.readOnlyNotice`) keeps copy and delete but cannot regenerate or edit-and-resend. Here rather than inline for `ChatBox`'s §3.5 complexity ceiling. */
export function buildMessageActions(readOnly: boolean, actions: {
  readonly onCopyToClipboard: ChatMessageListMessageActions['onCopyToClipboard'];
  readonly onDeleteAnswer: ChatMessageListMessageActions['onDeleteAnswer'];
  readonly onRegenerateAnswer: ChatMessageListMessageActions['onRegenerateAnswer'];
  readonly onSubmitEditedMessage: ChatMessageListMessageActions['onSubmitEditedMessage'];
}, noRegenerate = false): ChatMessageListMessageActions {
  if (readOnly) return { onCopyToClipboard: actions.onCopyToClipboard, onDeleteAnswer: actions.onDeleteAnswer };
  // Regenerate is dropped (edit-and-resend stays) while the newest answer ran on a desktop.
  if (noRegenerate) return { onCopyToClipboard: actions.onCopyToClipboard, onDeleteAnswer: actions.onDeleteAnswer, onSubmitEditedMessage: actions.onSubmitEditedMessage };
  return actions;
}

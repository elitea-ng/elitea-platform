/**
 * Public API — the conversation list's data and actions: the sidebar's chat
 * list (`widgets/chat-rail`) reads `useConversationRail` (folders, date
 * groups, pins, "Show more", the row actions) and drags with
 * `useDragAndDrop`; the chat page marks a conversation read and shows its
 * activity; the archived page restores; Fork is shared with the chat page.
 * `ShareLinkDialog` is the "Share by link" dialog a row opens.
 */
export type { FolderListItem } from './lib/hooks/conversationListState.types';

export { useArchiveConversation } from './lib/hooks/useArchiveConversation.hooks';
export { useConversationActivity } from './lib/hooks/useConversationActivity.hooks';
export { useMarkConversationReadOnView } from './lib/hooks/useConversationActivityLive.hooks';
export { useConversationRail } from './lib/hooks/useConversationRail.hooks';
export { useDragAndDrop } from './lib/hooks/useDragAndDrop';
export { useForkConversation } from './lib/hooks/useForkConversation.hooks';
export { ShareLinkDialog } from './ui/ShareLinkDialog';

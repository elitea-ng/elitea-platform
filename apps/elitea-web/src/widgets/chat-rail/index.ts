/**
 * Public API — the sidebar's chat list and place rows, one for the web
 * sidebar (`widgets/sidebar`) and the desktop's (`widgets/desktop-shell`):
 * the chat rail model and its provider, the Chats caption and list
 * (`ChatsSection`), the row primitives with their "+" create affordance, the
 * place rows with the Workshop group (`NavRows`). What only the desktop has (its notice
 * banner, the native context menu) comes in as props.
 */
export { ARCHIVED_PATH, ChatRailProvider, findInRail, openConversationId, useChatRail } from './model/chatRail';
export type { ContextAction } from './model/contextAction';
export { useChatsSectionOpen } from './model/useSectionOpen';
export { conversationLink, copyText } from './lib/conversationLink';
export { ChatsCaption, useStartChat } from './ui/ChatsCaption';
export { ChatsSection } from './ui/ChatsSection';
export { RowMenu } from './ui/RowMenu';
export { CHILD_INDENT, SectionCaption, ShellRow } from './ui/rows';
export { FolderClosedIcon, FolderOpenIcon } from './ui/folderIcons';
export { NAV_ICONS, NavRows } from './ui/NavRows';

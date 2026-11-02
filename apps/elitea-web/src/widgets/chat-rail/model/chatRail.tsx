/**
 * The desktop's ONE chat list model (`useConversationRail`), shared by the
 * sidebar's Chats rows and the main title row. The title names the open
 * conversation and its folder from the same rows the sidebar selects, so the
 * two always agree, and a rename from either is one change.
 *
 * The frame provides it while the person can use the chat (the Chats row is
 * in the sidebar's list). `ChatsSection` outside a provider (its own tests)
 * holds a rail of its own.
 */
import { createContext, useContext, type ReactNode } from 'react';

import type { Conversation } from '@/entities/conversation';
import { useConversationRail } from '@/features/chat-conversation-list';

export type ChatRail = ReturnType<typeof useConversationRail>;

const ChatRailContext = createContext<ChatRail | null>(null);

function RailHolder({ projectId, children }: { readonly projectId: string | undefined; readonly children: ReactNode }): React.JSX.Element {
  const rail = useConversationRail({ projectId });
  return <ChatRailContext.Provider value={rail}>{children}</ChatRailContext.Provider>;
}

export function ChatRailProvider({ projectId, enabled, children }: { readonly projectId: string | undefined; readonly enabled: boolean; readonly children: ReactNode }): React.JSX.Element {
  return enabled ? <RailHolder projectId={projectId}>{children}</RailHolder> : <>{children}</>;
}

/** The frame's chat rail, or `null` outside a provider (or without the chat). */
export function useChatRail(): ChatRail | null {
  return useContext(ChatRailContext);
}

export const ARCHIVED_PATH = '/chat/archived';

/** The open conversation, from the route (`/chat/<id>`); `''` elsewhere. */
export function openConversationId(pathname: string): string {
  const match = /^\/chat\/([^/]+)\/?$/.exec(pathname);
  return match?.[1] === undefined || pathname.startsWith(ARCHIVED_PATH) ? '' : decodeURIComponent(match[1]);
}

/** Where a conversation is in the rail: its row, and the folder or date group that holds it. */
export interface RailPlace {
  readonly conversation: Conversation;
  readonly folder?: { readonly id: string; readonly name: string };
  /** The date group's name; `pinned` for a pinned row. */
  readonly group?: string;
}

export const PINNED_GROUP = 'pinned';

export function findInRail(rail: ChatRail | null, conversationId: string): RailPlace | undefined {
  if (rail === null || conversationId === '') return undefined;
  const pinned = rail.pinned.find((item) => item.id === conversationId);
  if (pinned !== undefined) return { conversation: pinned, group: PINNED_GROUP };
  for (const folder of rail.folders) {
    const found = folder.conversations.find((item) => item.id === conversationId);
    if (found !== undefined) return { conversation: found, folder: { id: folder.id, name: folder.name } };
  }
  for (const group of rail.dateGroups) {
    const found = group.conversations.find((item) => item.id === conversationId);
    if (found !== undefined) return { conversation: found, group: group.name };
  }
  return undefined;
}

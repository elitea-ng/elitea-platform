/**
 * The ONE chat list model (`useConversationRail`) of a sidebar, web or
 * desktop: the sidebar's Chats rows read it, and on the desktop the main
 * title row too. The title names the open conversation and its folder from
 * the same rows the sidebar selects, so the two always agree, and a rename
 * from either is one change.
 *
 * The provider also holds the list's filter: the title search the Chats
 * caption opens (`query`, sent to the listing a moment after the last
 * keystroke) and the web's "Local work only" switch.
 *
 * The shell provides it while the person can use the chat (the Chats row is
 * in the sidebar's list). `ChatsSection` outside a provider (its own tests)
 * holds a rail of its own.
 *
 * The rail's state (rows, search, filter) lives as long as the provider, so
 * collapsing the sidebar or the Chats list keeps it. A `lazy` provider (the
 * web sidebar, mounted on every page) reads the listing and follows its live
 * stream only while a Chats list is on screen (`useRailDemand`); otherwise it
 * reads whenever it is `enabled` (the desktop, whose title row names the open
 * conversation from the rail).
 */
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';

import type { Conversation } from '@/entities/conversation';
import { useConversationRail } from '@/features/chat-conversation-list';
import { useDebounce } from '@/shared/lib/useDebounce';

export type ChatRail = ReturnType<typeof useConversationRail>;

export interface ChatRailFilter {
  /** The search field is open (the Chats caption's search button). */
  readonly searchOpen: boolean;
  readonly setSearchOpen: (open: boolean) => void;
  readonly query: string;
  readonly setQuery: (query: string) => void;
  readonly localWorkOnly: boolean;
  readonly setLocalWorkOnly: (only: boolean) => void;
}

const SEARCH_DEBOUNCE_MS = 300;

const ChatRailContext = createContext<ChatRail | null>(null);
const ChatRailFilterContext = createContext<ChatRailFilter | null>(null);
/** Registers one on-screen reader of a lazy rail; returns its release. */
const RailDemandContext = createContext<(() => () => void) | null>(null);

export interface ChatRailProviderProps {
  readonly projectId: string | undefined;
  /** Whether the person can use the chat here at all; `false` provides no rail. */
  readonly enabled: boolean;
  /** Read only while a Chats list is on screen (`useRailDemand`). */
  readonly lazy?: boolean;
  readonly children: ReactNode;
}

export function ChatRailProvider({ projectId, enabled, lazy = false, children }: ChatRailProviderProps): React.JSX.Element {
  const [readers, setReaders] = useState(0);
  const demand = useCallback(() => {
    setReaders((count) => count + 1);
    return () => setReaders((count) => count - 1);
  }, []);
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [localWorkOnly, setLocalWorkOnly] = useState(false);
  const searchQuery = useDebounce(searchOpen ? query.trim() : '', SEARCH_DEBOUNCE_MS);
  const rail = useConversationRail({ projectId, searchQuery, localWorkOnly, enabled: enabled && (!lazy || readers > 0) });
  const filter = useMemo(() => ({ searchOpen, setSearchOpen, query, setQuery, localWorkOnly, setLocalWorkOnly }), [searchOpen, query, localWorkOnly]);
  if (!enabled) return <>{children}</>;
  return (
    <RailDemandContext.Provider value={demand}>
      <ChatRailFilterContext.Provider value={filter}>
        <ChatRailContext.Provider value={rail}>{children}</ChatRailContext.Provider>
      </ChatRailFilterContext.Provider>
    </RailDemandContext.Provider>
  );
}

/** A Chats list on screen: a lazy rail reads (and follows its stream) while one is mounted. */
export function useRailDemand(): void {
  const demand = useContext(RailDemandContext);
  useEffect(() => demand?.(), [demand]);
}

/** The shell's chat rail, or `null` outside a provider (or without the chat). */
export function useChatRail(): ChatRail | null {
  return useContext(ChatRailContext);
}

/** The rail's filter, or `null` outside a provider. */
export function useChatRailFilter(): ChatRailFilter | null {
  return useContext(ChatRailFilterContext);
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

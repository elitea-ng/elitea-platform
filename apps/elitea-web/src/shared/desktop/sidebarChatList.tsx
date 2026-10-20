/**
 * Whether the desktop frame's sidebar shows the conversations right now (the
 * rows under its Chats row).
 *
 * The chat page drops its in-page conversation list only while that sidebar
 * list is on screen. The list is gone when the sidebar is hidden, or when the
 * Elitea section or the Chats disclosure is collapsed; the page then shows its
 * own list, so there is always one list of conversations.
 *
 * The frame (`widgets/desktop-shell`) provides this context; the sidebar's
 * list announces itself with `useAnnounceSidebarChatList`. Outside the desktop
 * frame (the web build) there is no context, `useSidebarChatList()` is `null`,
 * and the page keeps its list.
 */
import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useState, type ReactNode } from 'react';

export interface SidebarChatList {
  /** A sidebar list of conversations is mounted. */
  readonly mounted: boolean;
  /** The sidebar list announces itself; the returned function withdraws it. */
  readonly register: () => () => void;
}

const SidebarChatListContext = createContext<SidebarChatList | null>(null);

/** The frame's side of the signal: holds how many sidebar lists are mounted. */
export function SidebarChatListProvider({ children }: { readonly children: ReactNode }): React.JSX.Element {
  const [count, setCount] = useState(0);
  const register = useCallback(() => {
    setCount((current) => current + 1);
    return () => setCount((current) => current - 1);
  }, []);
  const value = useMemo(() => ({ mounted: count > 0, register }), [count, register]);
  return <SidebarChatListContext.Provider value={value}>{children}</SidebarChatListContext.Provider>;
}

/** The frame's sidebar list signal, or `null` outside the desktop frame. */
export function useSidebarChatList(): SidebarChatList | null {
  return useContext(SidebarChatListContext);
}

/** Marks the sidebar's conversation list as mounted while the caller is. */
export function useAnnounceSidebarChatList(): void {
  const register = useSidebarChatList()?.register;
  // A layout effect: the page drops its own list before the first paint, so the two never show together.
  useLayoutEffect(() => register?.(), [register]);
}

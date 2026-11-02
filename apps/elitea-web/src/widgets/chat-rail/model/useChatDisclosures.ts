/**
 * Which groups and folders of the sidebar's Chats list are open: the
 * remembered state of each date group (`useChatGroupsOpen`), the folders
 * opened this session, and the one that holds the open conversation. That
 * one opens for it at each navigation, or when the conversation moves to
 * another holder (a turn moves it to Today), until the person closes it.
 */
import { useEffect, useState } from 'react';

import { findInRail, type ChatRail } from './chatRail';
import { chatGroupKey, useChatGroupsOpen } from './useEliteaSectionOpen';

export function useChatDisclosures(rail: ChatRail, openId: string) {
  const groups = useChatGroupsOpen();
  const [openFolders, setOpenFolders] = useState<ReadonlySet<string>>(new Set());
  const place = findInRail(rail, openId);
  const holder = place === undefined ? undefined : place.folder !== undefined ? `folder:${place.folder.id}` : `group:${chatGroupKey(place.group ?? '')}`;
  const target = holder === undefined ? undefined : `${openId}|${holder}`;
  const [revealed, setRevealed] = useState<string | undefined>(undefined);
  useEffect(() => {
    if (target !== undefined) setRevealed(target);
  }, [target]);
  const isRevealed = (key: string): boolean => revealed === `${openId}|${key}`;
  const setFolder = (id: string, open: boolean): void =>
    setOpenFolders((current) => {
      const next = new Set(current);
      if (open) next.add(id);
      else next.delete(id);
      return next;
    });
  return {
    groupOpen: (name: string) => groups.isOpen(chatGroupKey(name)) || isRevealed(`group:${chatGroupKey(name)}`),
    toggleGroup: (name: string) => {
      const key = chatGroupKey(name);
      // Open only for the open conversation: closing it is not a remembered change.
      if (!groups.isOpen(key) && isRevealed(`group:${key}`)) setRevealed(undefined);
      else groups.toggle(key);
    },
    folderOpen: (id: string) => openFolders.has(id) || isRevealed(`folder:${id}`),
    toggleFolder: (id: string) => {
      if (!openFolders.has(id) && isRevealed(`folder:${id}`)) setRevealed(undefined);
      else setFolder(id, !openFolders.has(id));
    },
    expandFolder: (id: string) => setFolder(id, true),
  };
}


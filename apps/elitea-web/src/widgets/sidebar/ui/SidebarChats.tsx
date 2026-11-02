/**
 * The web rail's Chats: the caption — search, a new folder, a new chat, the
 * "⋯" with the Local work filter, the disclosure — and the list under it
 * (Pinned, folders, date groups, the Archived row). Both are the desktop
 * sidebar's own (`widgets/chat-rail`); the list's data comes from the
 * `ChatRailProvider` the rail is wrapped in (`Sidebar.tsx`).
 */
import { useState, type ReactNode } from 'react';

import { PERMISSIONS } from '@/shared/lib/permissions';
import { ChatsCaption, ChatsSection, useChatsSectionOpen, useStartChat } from '@/widgets/chat-rail';

export interface SidebarChatsProps {
  readonly permissions: ReadonlySet<string>;
  readonly projectId: string | undefined;
  readonly pathname: string;
  /** Whether a new chat may be started here (`useCreateGate`). */
  readonly canCreateChat: boolean;
}

export function SidebarChats({ permissions, projectId, pathname, canCreateChat }: SidebarChatsProps): ReactNode {
  const [open, toggle, expand] = useChatsSectionOpen(pathname);
  const newChat = useStartChat();
  const [creatingFolder, setCreatingFolder] = useState(false);
  const onNewFolder = permissions.has(PERMISSIONS.chat.folders.create)
    ? () => {
        expand();
        setCreatingFolder(true);
      }
    : undefined;
  return (
    <>
      <ChatsCaption open={open} onToggle={toggle} onExpand={expand} onNewFolder={onNewFolder} onNewChat={canCreateChat ? newChat : undefined} localWorkMenu />
      {open && (
        <ChatsSection
          projectId={projectId}
          pathname={pathname}
          creatingFolder={creatingFolder}
          onCreatingFolderDone={() => setCreatingFolder(false)}
          canArchive={permissions.has(PERMISSIONS.chat.archive)}
          canMoveToFolder={permissions.has(PERMISSIONS.chat.folders.update)}
          canDeleteFolder={permissions.has(PERMISSIONS.chat.folders.delete)}
        />
      )}
    </>
  );
}

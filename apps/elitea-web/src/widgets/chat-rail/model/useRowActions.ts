/**
 * The Chats list's row actions (`ConversationRowActions`) over the rail: the
 * route opens a conversation (a fork or a duplicate opens at once, playback
 * opens it with `playback=1`), a deleted open conversation leaves for the
 * blank chat, and the visibility switch is offered outside the public project
 * and the person's own.
 */
import { useNavigate } from '@tanstack/react-router';

import { isPublicProject } from '@/entities/project';
import { useForkConversation } from '@/features/chat-conversation-list';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import { getConfig } from '@/shared/config';

import { conversationShareLink, copyText } from '../lib/conversationLink';
import type { ConversationRowActions } from '../lib/conversationMenu';
import type { ChatRail } from './chatRail';

export interface RowRights {
  readonly canArchive: boolean;
  readonly canMoveToFolder: boolean;
}

/** Neither the public project nor the person's own: a chat in it may be public. */
function useCanChangeVisibility(projectId: string | undefined): boolean {
  const author = useGetCurrentAuthor();
  const personal = (unwrapBody(author.data) as SocialAuthorProfile | undefined)?.personal_project_id;
  if (projectId === undefined) return false;
  const config = getConfig();
  if (config.status === 'ok' && isPublicProject(projectId, config.config.vite_public_project_id)) return false;
  return personal === undefined || String(personal) !== projectId;
}

export function useRowActions(rail: ChatRail, openId: string, projectId: string | undefined, rights: RowRights, onError: (message: string) => void): ConversationRowActions {
  const navigate = useNavigate();
  const fork = useForkConversation({ projectId, toastError: onError });
  const canChangeVisibility = useCanChangeVisibility(projectId);
  const openChat = (conversationId: string): void => void navigate({ to: '/chat/$conversationId', params: { conversationId } });
  return {
    open: (conversation) => openChat(conversation.id),
    pin: rail.pin,
    rename: rail.rename,
    canModify: rail.canModify,
    canArchive: rights.canArchive,
    canMoveToFolder: rights.canMoveToFolder,
    canChangeVisibility,
    // The open conversation stays open when it is archived (an archive changes the lists, not the chat).
    archive: rail.archive,
    fork: (conversation) =>
      void fork(conversation.id).then((made) => {
        if (made !== undefined) openChat(made.id);
      }),
    duplicate: (conversation) =>
      void rail.duplicate(conversation).then((id) => {
        if (id !== undefined) openChat(id);
      }),
    exportAs: rail.exportAs,
    playback: (conversation) => void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id }, search: { playback: '1' } as never }),
    setPrivate: rail.setPrivate,
    remove: (conversation) =>
      void rail.remove(conversation).then((removed) => {
        if (removed && conversation.id === openId) void navigate({ to: '/chat' });
      }),
    folders: rail.folders.map((folder) => ({ id: folder.id, name: folder.name })),
    folderOf: (conversation) => rail.folders.find((folder) => folder.conversations.some((item) => item.id === conversation.id))?.id ?? null,
    moveToFolder: (conversation, folderId) => void rail.moveToFolder(conversation, folderId),
    linkOf: (conversation) => conversationShareLink(conversation, projectId),
    copy: copyText,
  };
}

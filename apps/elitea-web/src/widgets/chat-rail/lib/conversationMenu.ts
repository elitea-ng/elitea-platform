/**
 * A conversation row's actions, once, for both menus (the hover "⋯" and, on
 * the desktop, the native context menu): Open; Pin / Unpin, Rename, Move to
 * folder…, Duplicate, Fork, Save as team… (with the teams create right,
 * ADR-0050 decision 8), Export (Markdown or JSON), Playback; Make public
 * or Restrict access (not in the public project or the person's own), Share
 * by link…, Copy Link; Archive and Delete…. Rename and Delete are the
 * author's. A pinned chat is not moved.
 */
import type { Conversation } from '@/entities/conversation';
import { t } from '@/shared/i18n';

import type { ContextAction } from '../model/contextAction';

type ConversationExportFormat = 'md' | 'json';

export interface FolderChoice {
  readonly id: string;
  readonly name: string;
}

export interface ConversationRowActions {
  readonly open: (conversation: Conversation) => void;
  readonly pin: (conversation: Conversation, shouldPin: boolean) => void;
  readonly rename: (conversation: Conversation, name: string) => void;
  readonly archive: (conversation: Conversation) => void;
  /** Forks the whole history into a new chat of the caller, and opens it. */
  readonly fork: (conversation: Conversation) => void;
  /** Copies participants, settings and visibility into a new chat, and opens it. */
  readonly duplicate: (conversation: Conversation) => void;
  readonly exportAs: (conversation: Conversation, format: ConversationExportFormat) => void;
  /** Opens the conversation in playback. */
  readonly playback: (conversation: Conversation) => void;
  readonly setPrivate: (conversation: Conversation, isPrivate: boolean) => void;
  readonly remove: (conversation: Conversation) => void;
  readonly canModify: (conversation: Conversation) => boolean;
  /** Holds the project's archive right (`PERMISSIONS.chat.archive`); without it the server answers 403. */
  readonly canArchive: boolean;
  /** The project is neither the public one nor the person's own, so a chat in it can be public. */
  readonly canChangeVisibility: boolean;
  /** The project's chat folders, for Move to folder…. */
  readonly folders: readonly FolderChoice[];
  /** The folder that holds the conversation now; `null` when none does. */
  readonly folderOf: (conversation: Conversation) => string | null;
  readonly moveToFolder: (conversation: Conversation, folderId: string | null) => void;
  /** Holds the folder right (`PERMISSIONS.chat.folders.update`): Move to folder… and the drag. */
  readonly canMoveToFolder: boolean;
  /** The link Copy Link copies; `null` disables it. */
  readonly linkOf: (conversation: Conversation) => string | null;
  readonly copy: (text: string) => void;
  /** Holds `models.teams.create` (issue 1620): Save as team…, which saves the chat's agents and lead as a team. */
  readonly canSaveAsTeam: boolean;
}

/** What the row asks before it acts: each opens its own field or dialog. */
export interface RowAsks {
  readonly rename: () => void;
  readonly move: () => void;
  readonly visibility: () => void;
  readonly shareByLink: () => void;
  readonly archive: () => void;
  readonly remove: () => void;
  readonly saveAsTeam: () => void;
}

function visibilityAction(conversation: Conversation, actions: ConversationRowActions, asks: RowAsks): ContextAction[] {
  if (!actions.canChangeVisibility) return [];
  const label = conversation.isPrivate ? t('desktop.chats.makePublic', 'Make public') : t('desktop.chats.restrictAccess', 'Restrict access');
  return [{ id: 'visibility', label, enabled: actions.canModify(conversation), run: asks.visibility }];
}

export function conversationActions(conversation: Conversation, actions: ConversationRowActions, asks: RowAsks): ContextAction[] {
  const link = actions.linkOf(conversation);
  const pinned = conversation.isPinned === true;
  const mine = actions.canModify(conversation);
  return [
    { id: 'open', label: t('desktop.menu.open', 'Open'), run: () => actions.open(conversation) },
    { separator: true },
    { id: 'pin', label: pinned ? t('desktop.chats.unpin', 'Unpin') : t('desktop.chats.pin', 'Pin'), run: () => actions.pin(conversation, !pinned) },
    { id: 'rename', label: t('desktop.chats.rename', 'Rename'), enabled: mine, run: asks.rename },
    {
      id: 'move',
      label: t('desktop.chats.moveToFolder', 'Move to folder…'),
      enabled: actions.canMoveToFolder && !pinned && (actions.folders.length > 0 || actions.folderOf(conversation) !== null),
      run: asks.move,
    },
    { id: 'duplicate', label: t('desktop.chats.duplicate', 'Duplicate'), run: () => actions.duplicate(conversation) },
    { id: 'fork', label: t('desktop.chats.fork', 'Fork'), run: () => actions.fork(conversation) },
    ...(actions.canSaveAsTeam ? [{ id: 'save-as-team', label: t('features.agentTeams.saveAs.menu', 'Save as team…'), run: asks.saveAsTeam }] : []),
    {
      label: t('desktop.chats.export', 'Export'),
      submenu: [
        { id: 'export-md', label: t('desktop.chats.exportMarkdown', 'Markdown (.md)'), run: () => actions.exportAs(conversation, 'md') },
        { id: 'export-json', label: t('desktop.chats.exportJson', 'JSON (.json)'), run: () => actions.exportAs(conversation, 'json') },
      ],
    },
    { id: 'playback', label: t('desktop.chats.playback', 'Playback'), run: () => actions.playback(conversation) },
    { separator: true },
    ...visibilityAction(conversation, actions, asks),
    { id: 'share-by-link', label: t('desktop.chats.shareByLink', 'Share by link'), run: asks.shareByLink },
    { id: 'link', label: t('workspace.copyLink', 'Copy Link'), enabled: link !== null, run: () => link !== null && actions.copy(link) },
    { separator: true },
    { id: 'archive', label: t('desktop.chats.archive', 'Archive'), enabled: actions.canArchive, run: asks.archive },
    { id: 'delete', label: t('desktop.chats.delete', 'Delete…'), enabled: mine, run: asks.remove },
  ];
}

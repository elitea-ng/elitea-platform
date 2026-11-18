/**
 * One conversation in the sidebar's Chats list: a row (`ShellRow`) that
 * opens it, with its actions (`lib/conversationMenu.ts`) on the hover "⋯"
 * button and, on the desktop, on a right-click (the OS menu,
 * `onNativeContextMenu`). Rename edits the title in place; Move to folder…
 * lists the project's chat folders; Share by link… opens the share links
 * dialog. Archive, Delete and the visibility switch ask first: the archive is
 * the project's (it takes the chat out of every member's list; any member can
 * unarchive it).
 *
 * A row the person may move (not pinned, with the folder right) is a drag
 * source (`@dnd-kit`, `useDraggable` with the conversation id, the web
 * list's id scheme); `ChatsSection` owns the drop. The open conversation's
 * row scrolls into view when it becomes the selection.
 */
import { useEffect, useRef, useState, type KeyboardEvent, type MouseEvent } from 'react';

import { useDraggable } from '@dnd-kit/core';

import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';

import type { Conversation } from '@/entities/conversation';
import { SaveAsTeamDialog } from '@/features/agent-teams';
import { ShareLinkDialog } from '@/features/chat-conversation-list';
import { t } from '@/shared/i18n';
import { NAME_MAX_LENGTH, exceedsLimit, isNearLimit } from '@/shared/lib/textLimits';
import { CharacterCounter } from '@/shared/ui/CharacterCounter';
import { activityGlyph, activityTitleBold, useRowActivity } from '@/shared/lib/conversationActivity';
import { ConversationActivityGlyph } from '@/shared/ui/ConversationActivity';
import { BaseModal } from '@/shared/ui/BaseModal';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { conversationActions, type ConversationRowActions, type RowAsks } from '../lib/conversationMenu';
import type { ContextAction } from '../model/contextAction';
import { MoveToFolderDialog } from './MoveToFolderDialog';
import { RowMenu } from './RowMenu';
import { PinnedStatus } from './folderIcons';
import { ShellRow } from './rows';

/** The desktop's native context menu for a row; absent on the web, where the browser's own menu stays. */
export type NativeContextMenu = (event: MouseEvent, actions: readonly ContextAction[]) => void;

export interface ConversationRowProps {
  readonly conversation: Conversation;
  readonly selected: boolean;
  readonly indent: number;
  readonly actions: ConversationRowActions;
  readonly projectId: string | undefined;
  readonly onNativeContextMenu?: NativeContextMenu | undefined;
}

/** The title being edited in place: Enter saves, Escape or leaving the field cancels. */
function RenameField({ conversation, indent, onDone }: { readonly conversation: Conversation; readonly indent: number; readonly onDone: (name: string | null) => void }): React.JSX.Element {
  const [value, setValue] = useState(conversation.name);
  const tooLong = exceedsLimit(value, NAME_MAX_LENGTH);
  // Enter does not save a name past the limit (issue 1968); the counter says by how much.
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') {
      if (!tooLong) onDone(value.trim());
    } else if (event.key === 'Escape') onDone(null);
  };
  return (
    <Box sx={{ paddingLeft: 1 + indent, paddingRight: 0.5, minHeight: '1.75rem', display: 'flex', alignItems: 'center', flexWrap: 'wrap' }}>
      <InputBase
        value={value}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => onDone(null)}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{ 'aria-label': t('desktop.chats.renameLabel', 'Conversation name'), 'aria-invalid': tooLong || undefined, 'data-testid': 'shell-conversation-rename' }}
        sx={(theme: Theme) => ({
          flex: 1,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          border: `1px solid ${theme.vars.palette.primary.main}`,
          background: theme.vars.palette.background.default,
          typography: 'labelSmall',
        })}
      />
      <CharacterCounter
        value={value}
        maxLength={NAME_MAX_LENGTH}
        visible={isNearLimit(value, NAME_MAX_LENGTH)}
        textVariant="labelSmall"
        sx={{ flexBasis: '100%' }}
        data-testid="shell-conversation-rename-counter"
      />
    </Box>
  );
}

/** The row as a drag source, while it may move; the wrapper takes pointer events only (the keyboard way is Move to folder…). */
function useRowDrag(conversation: Conversation, actions: ConversationRowActions) {
  const movable = actions.canMoveToFolder && conversation.isPinned !== true;
  const { listeners, setNodeRef, isDragging } = useDraggable({ id: conversation.id, disabled: !movable });
  const onPointerDown = movable ? (listeners?.['onPointerDown'] as React.PointerEventHandler | undefined) : undefined;
  return { ref: setNodeRef, onPointerDown, isDragging };
}

/** The selected row scrolls into view: after a navigation, a refetch, or a group or folder that opened for it. */
function useScrollIntoViewWhenSelected(selected: boolean) {
  const node = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (selected) node.current?.scrollIntoView?.({ block: 'nearest' });
  }, [selected]);
  return node;
}

type Ask = 'rename' | 'move' | 'visibility' | 'shareByLink' | 'archive' | 'remove' | 'saveAsTeam';

interface RowDialogsProps {
  readonly conversation: Conversation;
  readonly label: string;
  readonly ask: Ask | null;
  readonly close: () => void;
  readonly actions: ConversationRowActions;
  readonly projectId: string | undefined;
}

/** The dialogs the row's actions ask through. */
function RowDialogs({ conversation, label, ask, close, actions, projectId }: RowDialogsProps): React.JSX.Element {
  const makePublic = conversation.isPrivate;
  const confirm = (run: () => void) => () => {
    close();
    run();
  };
  return (
    <>
      {ask === 'move' && (
        <MoveToFolderDialog open name={label} folders={actions.folders} currentFolderId={actions.folderOf(conversation)} onClose={close} onMove={(folderId) => actions.moveToFolder(conversation, folderId)} />
      )}
      {ask === 'shareByLink' && projectId !== undefined && <ShareLinkDialog open projectId={projectId} conversationId={conversation.id} conversationName={label} onClose={close} />}
      <BaseModal
        open={ask === 'visibility'}
        title={makePublic ? t('desktop.chats.makePublic', 'Make public') : t('desktop.chats.restrictAccess', 'Restrict access')}
        content={
          makePublic
            ? t('desktop.chats.makePublicText', 'Everyone in the project can open this conversation.')
            : t('desktop.chats.restrictAccessText', 'Project members who are not participants will no longer be able to open this conversation.')
        }
        onClose={close}
        onConfirm={confirm(() => actions.setPrivate(conversation, !makePublic))}
        actions={{ confirmText: makePublic ? t('desktop.chats.makePublic', 'Make public') : t('desktop.chats.restrictAccess', 'Restrict access') }}
      />
      <BaseModal
        open={ask === 'archive'}
        title={t('desktop.chats.archiveTitle', 'Archive “{{name}}”?', { name: label })}
        content={t('desktop.chats.archiveText', "The chat moves to the project's Archived chats and leaves the chat list for everyone in the project. Any member can unarchive it.")}
        onClose={close}
        onConfirm={confirm(() => actions.archive(conversation))}
        actions={{ confirmText: t('desktop.chats.archive', 'Archive') }}
      />
      {projectId !== undefined && (
        <SaveAsTeamDialog projectId={projectId} conversationId={conversation.id} defaultName={conversation.name} open={ask === 'saveAsTeam'} onClose={close} />
      )}
      <DeleteEntityModal
        open={ask === 'remove'}
        name={label}
        onClose={close}
        onConfirm={confirm(() => actions.remove(conversation))}
        copy={{ title: t('desktop.chats.deleteTitle', 'Delete chat'), textContent: t('desktop.chats.deleteText', "This can't be undone. Delete ") }}
      />
    </>
  );
}

export function ConversationRow({ conversation, selected, indent, actions, projectId, onNativeContextMenu }: ConversationRowProps): React.JSX.Element {
  const drag = useRowDrag(conversation, actions);
  const scrollTarget = useScrollIntoViewWhenSelected(selected);
  const [ask, setAsk] = useState<Ask | null>(null);
  const label = conversation.name === '' ? t('desktop.chats.untitled', 'Untitled chat') : conversation.name;
  const asks: RowAsks = {
    rename: () => setAsk('rename'),
    move: () => setAsk('move'),
    visibility: () => setAsk('visibility'),
    shareByLink: () => setAsk('shareByLink'),
    archive: () => setAsk('archive'),
    remove: () => setAsk('remove'),
    saveAsTeam: () => setAsk('saveAsTeam'),
  };
  const items = conversationActions(conversation, actions, asks);
  // Client contract 1.12: the activity glyph and the bold title of an unread answer.
  const activity = useRowActivity(conversation);
  const close = (): void => setAsk(null);

  return (
    <>
      {ask === 'rename' ? (
        <RenameField
          conversation={conversation}
          indent={indent}
          onDone={(name) => {
            close();
            if (name !== null && name !== '') actions.rename(conversation, name);
          }}
        />
      ) : (
        <Box
          ref={(element: HTMLDivElement | null) => {
            drag.ref(element);
            scrollTarget.current = element;
          }}
          data-conversation-id={conversation.id}
          onPointerDown={drag.onPointerDown}
          sx={{ opacity: drag.isDragging ? 0.5 : 1 }}
        >
          <ShellRow
            testId="shell-conversation"
            label={label}
            title={label}
            indent={indent}
            selected={selected}
            onClick={() => actions.open(conversation)}
            {...(onNativeContextMenu !== undefined ? { onContextMenu: (event: MouseEvent) => onNativeContextMenu(event, items) } : {})}
            bold={activityTitleBold(activity)}
            status={<PinnedStatus pinned={conversation.isPinned === true}><ConversationActivityGlyph kind={activityGlyph(activity)} /></PinnedStatus>}
            trailingOnHover
            trailing={<RowMenu items={items} label={t('desktop.chats.more', 'More for {{name}}', { name: label })} />}
          />
        </Box>
      )}
      <RowDialogs conversation={conversation} label={label} ask={ask} close={close} actions={actions} projectId={projectId} />
    </>
  );
}

/**
 * The parts of the sidebar's Chats list (`ChatsSection.tsx`): a date group
 * with its disclosure caption, the conversation rows, a folder row (a drop
 * target), the recent chats' drop area, the new-folder field and the drag
 * preview. Their own file for the 400-line budget.
 */
import { useEffect, useState, type KeyboardEvent, type ReactNode } from 'react';

import { DragOverlay, useDroppable } from '@dnd-kit/core';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';

import type { Conversation } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { isLocalWorkConversation } from '@/shared/lib/localWork';

import type { ChatRail } from '../model/chatRail';
import { folderDropId, UNGROUPED_DROP_ID, type useSidebarChatDrag } from '../model/useSidebarChatDrag';
import { ConversationRow, type ConversationRowActions } from './ConversationRow';
import { CHILD_INDENT, ShellRow } from './rows';

/** How long a dragged row hovers a closed folder before it opens. */
const OPEN_ON_HOVER_MS = 600;

export const LIST_SX = { margin: 0, padding: 0, listStyle: 'none' } as const;

interface GroupProps {
  readonly caption: string;
  readonly count: number;
  readonly open: boolean;
  readonly onToggle: () => void;
  readonly children: ReactNode;
}

/**
 * A date group, drawn as Local work draws a folder: the chevron in the icon
 * column, the caption on the text edge its chats share, the count at the right
 * while it is closed.
 */
export function Group({ caption, count, open, onToggle, children }: GroupProps): React.JSX.Element {
  return (
    <li>
      <Box component="h3" sx={{ margin: 0, font: 'inherit' }}>
        <ShellRow
          testId="shell-chats-group"
          muted
          icon={open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
          ariaExpanded={open}
          label={caption}
          labelClassName="shell-group-caption"
          metaTestId="shell-chats-group-count"
          {...(open ? {} : { meta: String(count) })}
          onClick={onToggle}
        />
      </Box>
      {open && (
        <Box component="ul" sx={LIST_SX}>
          {children}
        </Box>
      )}
    </li>
  );
}

interface RowsProps {
  readonly conversations: readonly Conversation[];
  readonly openId: string;
  readonly indent: number;
  readonly actions: ConversationRowActions;
}

export function Rows({ conversations, openId, indent, actions }: RowsProps): React.JSX.Element {
  // The listing already leaves Local work threads out; a row that still says so belongs to the Local work section above.
  return (
    <>
      {conversations.filter((conversation) => !isLocalWorkConversation(conversation)).map((conversation) => (
        <li key={conversation.id}>
          <ConversationRow conversation={conversation} selected={conversation.id === openId} indent={indent} actions={actions} />
        </li>
      ))}
    </>
  );
}

export function ShowMore({ indent, onClick }: { readonly indent: number; readonly onClick: () => void }): React.JSX.Element {
  return (
    <li>
      <ShellRow testId="shell-chats-more" muted indent={indent} label={t('desktop.chats.showMore', 'Show more')} onClick={onClick} />
    </li>
  );
}

export type Drag = ReturnType<typeof useSidebarChatDrag>;

interface FolderProps {
  readonly rail: ChatRail;
  readonly folder: ChatRail['folders'][number];
  readonly expanded: boolean;
  readonly onToggle: () => void;
  readonly onExpand: () => void;
  readonly openId: string;
  readonly actions: ConversationRowActions;
  readonly drag: Drag;
}

/** A folder row (a drop target), and its conversations while it is open. A dragged row that hovers a closed folder opens it. */
export function FolderItem({ rail, folder, expanded, onToggle, onExpand, openId, actions, drag }: FolderProps): React.JSX.Element {
  const id = folderDropId(folder.id);
  const { isOver, setNodeRef } = useDroppable({ id, disabled: !actions.canMoveToFolder });
  const dropActive = isOver && drag.getDropAreaState(id).isValidDropTarget;
  useEffect(() => {
    if (!dropActive || expanded) return undefined;
    const timer = window.setTimeout(onExpand, OPEN_ON_HOVER_MS);
    return () => window.clearTimeout(timer);
  }, [dropActive, expanded, onExpand]);
  const count = folder.total ?? folder.conversations.length;
  return (
    <li>
      <Box ref={setNodeRef}>
        <ShellRow
          testId="shell-chats-folder"
          icon={expanded ? <ExpandMoreIcon /> : <ChevronRightIcon />}
          ariaExpanded={expanded}
          label={folder.name}
          meta={String(count)}
          dropActive={dropActive}
          onClick={onToggle}
        />
      </Box>
      {expanded && (
        <Box component="ul" sx={LIST_SX}>
          <Rows conversations={folder.conversations} openId={openId} indent={CHILD_INDENT} actions={actions} />
          {rail.hasMore(folder) && <ShowMore indent={CHILD_INDENT} onClick={() => rail.loadMoreInFolder(folder.id)} />}
        </Box>
      )}
    </li>
  );
}

/** The recent chats: a drop target that takes a row out of its folder. */
export function RecentArea({ drag, children }: { readonly drag: Drag; readonly children: ReactNode }): React.JSX.Element {
  const { isOver, setNodeRef } = useDroppable({ id: UNGROUPED_DROP_ID });
  const valid = drag.getDropAreaState(UNGROUPED_DROP_ID).isValidDropTarget;
  return (
    <li>
      <Box
        ref={setNodeRef}
        data-testid="shell-chats-recent"
        sx={(theme: Theme) => ({
          borderRadius: theme.vars.shape.radiusSm,
          outline: isOver && valid ? `1px solid ${theme.vars.palette.primary.main}` : undefined,
          outlineOffset: -1,
        })}
      >
        <Box component="ul" sx={LIST_SX}>
          {children}
          {valid && (
            <li>
              <ShellRow testId="shell-chats-drop-out" muted indent={CHILD_INDENT} label={t('desktop.chats.dropOutOfFolder', 'Drop here to take it out of the folder')} onClick={() => undefined} />
            </li>
          )}
        </Box>
      </Box>
    </li>
  );
}

/** The new folder's name: Enter creates it, Escape or leaving the field cancels. */
export function NewFolderField({ onDone }: { readonly onDone: (name: string | null) => void }): React.JSX.Element {
  const [value, setValue] = useState('');
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'Enter') onDone(value.trim());
    else if (event.key === 'Escape') onDone(null);
  };
  return (
    <Box component="li" sx={(theme: Theme) => ({ paddingLeft: 1, paddingRight: 0.5, minHeight: '1.75rem', display: 'flex', alignItems: 'center', gap: 1, color: theme.vars.palette.text.metrics })}>
      <FolderOutlinedIcon aria-hidden sx={{ width: '1rem', height: '1rem' }} />
      <InputBase
        value={value}
        placeholder={t('desktop.chats.newFolderPlaceholder', 'Folder name')}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => onDone(null)}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{ 'aria-label': t('desktop.chats.newFolderLabel', 'New folder name'), 'data-testid': 'shell-chats-new-folder-name' }}
        sx={(theme: Theme) => ({
          flex: 1,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          border: `1px solid ${theme.vars.palette.primary.main}`,
          background: theme.vars.palette.background.default,
          typography: 'labelSmall',
        })}
      />
    </Box>
  );
}

/** The dragged row, following the pointer. */
export function DragPreview({ drag }: { readonly drag: Drag }): React.JSX.Element {
  const dragged = drag.draggedItems[0];
  return (
    <DragOverlay dropAnimation={null}>
      {dragged === undefined ? null : (
        <Box
          data-testid="shell-chats-drag-preview"
          sx={(theme: Theme) => ({
            paddingX: 1,
            minHeight: '1.75rem',
            display: 'flex',
            alignItems: 'center',
            maxWidth: '14rem',
            borderRadius: theme.vars.shape.radiusSm,
            background: theme.vars.palette.background.secondary,
            boxShadow: theme.vars.shadows[4],
            typography: 'labelSmall',
            color: theme.vars.palette.text.secondary,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          })}
        >
          {dragged.name === '' ? t('desktop.chats.untitled', 'Untitled chat') : dragged.name}
        </Box>
      )}
    </DragOverlay>
  );
}


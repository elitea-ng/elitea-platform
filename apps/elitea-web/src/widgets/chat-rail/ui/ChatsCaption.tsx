/**
 * The sidebar's Chats caption (web and desktop): the name, which starts a new
 * chat, and its actions — search the chats, a new folder, a new chat and the
 * disclosure of the list. The web adds a "⋯" menu with the "Local work only"
 * switch (its Local work threads have no section of their own there).
 *
 * A new chat is `/chat?create=1`, also on `/chat` itself
 * (`processes/chat/ui/useCreateChatReset.ts`). Without the right to create a
 * chat or a folder, its button is left out.
 */
import { useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import CheckIcon from '@mui/icons-material/Check';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import SearchIcon from '@mui/icons-material/Search';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { MenuRow, MenuRowIcon, MenuRowText, pressToOpen } from '@/shared/ui/native-menu';

import { useChatRailFilter, type ChatRailFilter } from '../model/chatRail';
import { SectionCaption } from './rows';

/** Starts a new chat. */
export function useStartChat(): () => void {
  const navigate = useNavigate();
  return () => void navigate({ to: '/chat', search: { create: '1' } as never });
}

function CaptionButton({ label, testId, onClick, expanded, children }: { readonly label: string; readonly testId: string; readonly onClick: () => void; readonly expanded?: boolean; readonly children: React.ReactNode }): React.JSX.Element {
  return (
    <Tooltip title={label}>
      <IconButton size="small" aria-label={label} aria-expanded={expanded} data-testid={testId} onClick={onClick}>
        {children}
      </IconButton>
    </Tooltip>
  );
}

function LocalWorkMenu({ filter }: { readonly filter: ChatRailFilter }): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const label = t('widgets.chatRail.more', 'More chat options');
  return (
    <>
      <IconButton size="small" aria-label={label} aria-haspopup="menu" data-testid="shell-chats-options" {...pressToOpen(setAnchor)}>
        <MoreHorizIcon fontSize="inherit" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} slotProps={{ list: { dense: true } }}>
        <MenuRow
          role="menuitemcheckbox"
          aria-checked={filter.localWorkOnly}
          data-testid="shell-chats-local-work-only"
          onClick={() => {
            setAnchor(null);
            filter.setLocalWorkOnly(!filter.localWorkOnly);
          }}
        >
          <MenuRowIcon>{filter.localWorkOnly ? <CheckIcon /> : null}</MenuRowIcon>
          <MenuRowText>{t('widgets.chatRail.localWorkOnly', 'Local work only')}</MenuRowText>
        </MenuRow>
      </Menu>
    </>
  );
}

export interface ChatsCaptionProps {
  readonly open: boolean;
  readonly onToggle: () => void;
  /** Opens the list (a search opens it too). */
  readonly onExpand: () => void;
  /** Absent without the folder create right. */
  readonly onNewFolder?: (() => void) | undefined;
  /** Absent without the chat create right. */
  readonly onNewChat?: (() => void) | undefined;
  /** The web's "⋯" with the Local work filter. */
  readonly localWorkMenu?: boolean;
}

export function ChatsCaption({ open, onToggle, onExpand, onNewFolder, onNewChat, localWorkMenu = false }: ChatsCaptionProps): React.JSX.Element {
  const filter = useChatRailFilter();
  const searchLabel = t('widgets.chatRail.search', 'Search chats');
  const disclosure = open ? t('desktop.chats.hide', 'Hide chats') : t('desktop.chats.show', 'Show chats');
  const toggleSearch = (): void => {
    if (filter === null) return;
    if (filter.searchOpen) filter.setQuery('');
    else onExpand();
    filter.setSearchOpen(!filter.searchOpen);
  };
  const actions = (
    <>
      {filter !== null && (
        <CaptionButton label={searchLabel} testId="shell-chats-search" onClick={toggleSearch} expanded={filter.searchOpen}>
          <SearchIcon fontSize="inherit" />
        </CaptionButton>
      )}
      {onNewFolder !== undefined && (
        <CaptionButton label={t('desktop.chats.newFolder', 'New folder')} testId="shell-chats-new-folder" onClick={onNewFolder}>
          <CreateNewFolderOutlinedIcon fontSize="inherit" />
        </CaptionButton>
      )}
      {onNewChat !== undefined && (
        <CaptionButton label={t('desktop.chats.new', 'New chat')} testId="shell-chats-new" onClick={onNewChat}>
          <AddOutlinedIcon fontSize="inherit" />
        </CaptionButton>
      )}
      {localWorkMenu && filter !== null && <LocalWorkMenu filter={filter} />}
      <CaptionButton label={disclosure} testId="shell-chats-toggle" onClick={onToggle} expanded={open}>
        {open ? <ExpandMoreIcon fontSize="inherit" /> : <ChevronRightIcon fontSize="inherit" />}
      </CaptionButton>
    </>
  );
  return (
    <SectionCaption testId="shell-chats-caption" action={actions} {...(onNewChat !== undefined ? { onOpen: onNewChat } : {})}>
      {t('widgets.sidebar.nav.chat', 'Chats')}
    </SectionCaption>
  );
}

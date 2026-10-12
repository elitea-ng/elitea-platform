import type { ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import { useTheme, type Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { NewFolderIcon } from '@/shared/ui/icons/new-folder-icon';

import { ConversationSearchButton } from './ConversationSearchButton';
import { LocalWorkFilterButton, type LocalWorkFilterButtonProps } from './LocalWorkFilterButton';
import { conversationsStyles, createFolderButtonSx, menuIconStyle, newFolderIconFill } from './Conversations.styles';

/**
 * The title/create-folder/search/collapse-toggle header, plus the
 * conditionally-shown search bar (`Conversations.jsx:496-663`) — split out
 * of `Conversations.tsx` purely to keep that file under the §3.5
 * `max-lines`/`complexity` budgets, the same "extract a render chunk into
 * its own function" technique `ConversationItem.row.tsx`'s own doc comment
 * explains.
 */
export interface ConversationsHeaderProps {
  readonly collapsed: boolean;
  readonly isSmallWindow: boolean;
  readonly hasFolderCreatePermission: boolean;
  readonly onCreateFolderExpanded: () => void;
  readonly onCreateFolderCollapsed: () => void;
  readonly onCollapsedToggle: () => void;
  readonly onSearchActivate: (active: boolean) => void;
  readonly isSearchActive: boolean;
  readonly searchQuery: string;
  readonly onSearchChange: (value: string) => void;
  readonly onSearchClear: () => void;
  /** The "Local work" filter toggle; absent hides it. */
  readonly localWork?: LocalWorkFilterButtonProps | undefined;
}

/** The `collapsed && !isSmallWindow` narrow-rail toolbar (`Conversations.jsx:591-643`) — extracted to its own function purely to keep `ConversationsHeader`'s own complexity under the §3.5 budget. */
function CollapsedToolbar(props: { readonly hasFolderCreatePermission: boolean; readonly collapsed: boolean; readonly onCreateFolderCollapsed: () => void; readonly onCollapsedToggle: () => void; readonly onSearchActivate: (active: boolean) => void; readonly createFolderLabel: string }): ReactNode {
  const { hasFolderCreatePermission, collapsed, onCreateFolderCollapsed, onCollapsedToggle, onSearchActivate, createFolderLabel } = props;
  const theme = useTheme();

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: theme.spacing(1), justifyContent: 'center', alignItems: 'center', width: '100%' }}>
      <Tooltip
        title={createFolderLabel}
        placement="top"
      >
        <Box component="span">
          <Button
            disabled={!hasFolderCreatePermission}
            onClick={onCreateFolderCollapsed}
            variant="elitea"
            color="secondary"
            aria-label={createFolderLabel}
            sx={createFolderButtonSx}
          >
            <NewFolderIcon
              style={menuIconStyle}
              fill={newFolderIconFill(theme, hasFolderCreatePermission)}
            />
          </Button>
        </Box>
      </Tooltip>
      <ConversationSearchButton
        collapsed={collapsed}
        onExpand={onCollapsedToggle}
        onSearchActivate={onSearchActivate}
      />
    </Box>
  );
}

/** The conditionally-shown search bar (`Conversations.jsx:645-663`) — extracted for the same reason as `CollapsedToolbar` above. */
function HeaderSearchBar(props: { readonly searchQuery: string; readonly onSearchChange: (value: string) => void; readonly onSearchClear: () => void }): ReactNode {
  const { searchQuery, onSearchChange, onSearchClear } = props;
  const theme = useTheme();
  const styles = conversationsStyles(theme);

  return (
    <Box sx={styles.searchBarContainer}>
      <SimpleSearchBar
        value={searchQuery}
        onChange={onSearchChange}
        onClear={onSearchClear}
        placeholder={t('features.chatConversationList.conversations.searchPlaceholder', 'Search conversations...')}
        data-testid="conversation-search-input"
      />
      <IconButton
        onClick={onSearchClear}
        color="tertiary"
        aria-label={t('features.chatConversationList.conversations.closeSearch', 'Close search')}
      >
        <CloseIcon fontSize="small" />
      </IconButton>
    </Box>
  );
}

export function ConversationsHeader(props: ConversationsHeaderProps): ReactNode {
  const { collapsed, isSmallWindow, hasFolderCreatePermission, onCreateFolderExpanded, onCreateFolderCollapsed, onCollapsedToggle, onSearchActivate, isSearchActive, searchQuery, onSearchChange, onSearchClear, localWork } = props;
  const theme = useTheme();
  const createFolderLabel = t('features.chatConversationList.conversations.createFolder', 'Create folder');
  const showExpandedControls = !collapsed || isSmallWindow;
  const showCollapsedToolbar = collapsed && !isSmallWindow;
  const showSearchBar = isSearchActive && !collapsed;

  return (
    <>
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'row',
          justifyContent: showCollapsedToolbar ? 'center' : 'space-between',
          height: '32px',
          alignItems: 'center',
        }}
      >
        <Box sx={{ display: 'flex', flexDirection: 'row', alignItems: 'center', gap: theme.spacing(1) }}>
          {showExpandedControls && <Typography variant="subtitle">{headerTitle(localWork)}</Typography>}
          {showExpandedControls && (
            <>
              <Tooltip
                title={createFolderLabel}
                placement="top"
              >
                <span>
                  <Button
                    disabled={!hasFolderCreatePermission}
                    onClick={onCreateFolderExpanded}
                    variant="elitea"
                    color="secondary"
                    aria-label={createFolderLabel}
                    sx={createFolderButtonSx}
                  >
                    <NewFolderIcon
                      style={menuIconStyle}
                      fill={newFolderIconFill(theme, hasFolderCreatePermission)}
                    />
                  </Button>
                </span>
              </Tooltip>
              <ConversationSearchButton
                collapsed={collapsed}
                onExpand={onCollapsedToggle}
                onSearchActivate={onSearchActivate}
              />
              <LocalWorkFilterButton localWork={localWork} />
            </>
          )}
        </Box>
        {!isSmallWindow && (
          <IconButton
            color="tertiary"
            onClick={onCollapsedToggle}
            aria-label={collapsed ? t('features.chatConversationList.conversations.expand', 'Expand sidebar') : t('features.chatConversationList.conversations.collapse', 'Collapse sidebar')}
          >
            {collapsed ? <KeyboardDoubleArrowRightIcon fontSize="small" sx={collapseIconSx} /> : <KeyboardDoubleArrowLeftIcon fontSize="small" sx={collapseIconSx} />}
          </IconButton>
        )}
      </Box>

      {showCollapsedToolbar && (
        <CollapsedToolbar
          hasFolderCreatePermission={hasFolderCreatePermission}
          collapsed={collapsed}
          onCreateFolderCollapsed={onCreateFolderCollapsed}
          onCollapsedToggle={onCollapsedToggle}
          onSearchActivate={onSearchActivate}
          createFolderLabel={createFolderLabel}
        />
      )}

      {showSearchBar && (
        <HeaderSearchBar
          searchQuery={searchQuery}
          onSearchChange={onSearchChange}
          onSearchClear={onSearchClear}
        />
      )}
    </>
  );
}

/** "Local work" while the filter is on, so the rail says which listing it shows. */
function headerTitle(localWork: LocalWorkFilterButtonProps | undefined): string {
  return localWork?.on === true
    ? t('features.chatConversationList.localWork.filter', 'Local work')
    : t('features.chatConversationList.conversations.title', 'Chats');
}

const collapseIconSx = (theme: Theme) => ({ color: theme.vars.palette.icon.fill.default });

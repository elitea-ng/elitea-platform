import { type ReactNode, useEffect, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { DATE_GROUP_DISPLAY_NAMES } from '../../lib/constants/conversationList.constants';
import type { DateGroupListItem } from '../../lib/hooks/conversationListState.types';
import { useDateGroupExpansion } from '../../lib/hooks/useDateGroupExpansion.hooks';
import type { DropAreaState } from '../../lib/hooks/useDragAndDrop';
import { DateGroup } from './DateGroup';
import type { RenderConversationItem } from './DateGroup';

/** Baseline `DATE_GROUP_DISPLAY_NAMES[group.name] || group.name` (`GroupedConversations.jsx:64-67`), rewritten as an `in`-guarded lookup so it type-checks against `DATE_GROUP_DISPLAY_NAMES`'s narrower key union without an unsafe index cast. */
function resolveDisplayName(groupName: string): string {
  if (groupName in DATE_GROUP_DISPLAY_NAMES) {
    return DATE_GROUP_DISPLAY_NAMES[groupName as keyof typeof DATE_GROUP_DISPLAY_NAMES];
  }
  return groupName;
}

export interface GroupedConversationsProps {
  readonly dateGroups: readonly DateGroupListItem[];
  readonly totalConversationsAmount: number;
  readonly renderConversationItem: RenderConversationItem;
  readonly onLoadMoreInGroup?: ((groupName: string) => void) | undefined;
  readonly loadingGroups?: ReadonlySet<string> | undefined;
  readonly enableDragAndDrop?: boolean | undefined;
  readonly getDropAreaState?: ((dropAreaId: string) => DropAreaState) | undefined;
  readonly selectedConversationId?: string | undefined;
  readonly isSearchMode?: boolean | undefined;
  readonly searchQuery?: string | undefined;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/ui/
 * groups/GroupedConversations.jsx` (unit C2) — the only component in this
 * cluster that calls `useDateGroupExpansion` directly, matching the
 * baseline's own structure (`DateGroup`/`DroppableGroupedArea` are both
 * pure presentational with respect to expansion/search-mode state).
 */
export function GroupedConversations({
  dateGroups,
  totalConversationsAmount,
  renderConversationItem,
  onLoadMoreInGroup,
  loadingGroups,
  enableDragAndDrop = false,
  getDropAreaState,
  selectedConversationId,
  isSearchMode = false,
  searchQuery = '',
}: GroupedConversationsProps): ReactNode {
  const visibleGroups = useMemo(() => dateGroups.filter((group) => group.conversations.length > 0), [dateGroups]);

  const { isGroupExpanded, toggleGroup, initializeExpansion, enterSearchMode, exitSearchMode } = useDateGroupExpansion();

  /*
   * Seeded as "not searching" and "no groups", NOT from this render's own
   * props, because a mount can already be in search mode: the rail's grouped
   * listing is keyed on the search query, so typing one puts the query into a
   * loading state and `Conversations.body.tsx` replaces this whole subtree
   * with skeletons until the filtered answer lands. What comes back is a fresh
   * mount holding the matches — and, seeded from its own props, this effect
   * saw no transition, did nothing, and `initializeExpansion` below is skipped
   * in search mode, so every group stayed collapsed. The rail narrowed
   * correctly and showed the user nothing.
   */
  const prevSearchModeRef = useRef(false);
  const prevSearchQueryRef = useRef(searchQuery);
  const prevGroupNamesRef = useRef('');

  useEffect(() => {
    const searchModeChanged = prevSearchModeRef.current !== isSearchMode;
    const searchQueryChanged = prevSearchQueryRef.current !== searchQuery;
    // By NAME, not by identity: every refetch hands down a new array, and
    // re-applying the expansion on each one would undo a group the user
    // collapsed by hand while the search was open.
    const groupNames = visibleGroups.map((group) => group.name).join('\u0000');
    const groupsChanged = prevGroupNamesRef.current !== groupNames;

    if (isSearchMode && (searchModeChanged || searchQueryChanged || groupsChanged)) {
      const lowerQuery = searchQuery.toLowerCase();
      // `conversation.name` is typed `string` (`entities/conversation/model/types.ts`)
      // but sourced from an explicitly unschemaed wire (`conversationApi.ts`'s own
      // doc comment) — the optional chain guards a nullish real value the type
      // doesn't rule out, matching baseline `GroupedConversations.jsx:41`'s own
      // `conv.name?.toLowerCase()`. Found missing by adversarial verify.
      const groupsWithMatches = visibleGroups.filter((group) => group.conversations.some((conversation) => Boolean(conversation.name?.toLowerCase().includes(lowerQuery))));
      enterSearchMode(groupsWithMatches.map((group) => group.name));
    } else if (!isSearchMode && searchModeChanged) {
      exitSearchMode();
    }

    prevSearchModeRef.current = isSearchMode;
    prevSearchQueryRef.current = searchQuery;
    prevGroupNamesRef.current = groupNames;
  }, [isSearchMode, searchQuery, visibleGroups, enterSearchMode, exitSearchMode]);

  useEffect(() => {
    if (!isSearchMode) initializeExpansion(visibleGroups, selectedConversationId);
  }, [visibleGroups, initializeExpansion, selectedConversationId, isSearchMode]);

  return (
    <>
      {visibleGroups.length > 0 && (
        <Box>
          {visibleGroups.map((group) => (
            <DateGroup
              key={group.name}
              group={{ ...group, displayName: resolveDisplayName(group.name) }}
              renderConversationItem={renderConversationItem}
              enableDragAndDrop={enableDragAndDrop}
              getDropAreaState={getDropAreaState}
              isExpanded={isGroupExpanded(group.name)}
              onToggleExpanded={toggleGroup}
              onLoadMore={() => onLoadMoreInGroup?.(group.name)}
              isLoadingMore={loadingGroups?.has(group.name)}
            />
          ))}
        </Box>
      )}
      {visibleGroups.length === 0 && totalConversationsAmount === 0 && (
        <Typography
          variant="bodyMedium"
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.button.disabled })}
        >
          {t('features.chatConversationList.groupedConversations.emptyState', 'Still no conversations created.')}
        </Typography>
      )}
    </>
  );
}

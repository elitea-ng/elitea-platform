import { type ReactNode, useCallback, useState } from 'react';

import ArrowForwardIosSharpIcon from '@mui/icons-material/ArrowForwardIosSharp';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Collapse from '@mui/material/Collapse';
import Skeleton from '@mui/material/Skeleton';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import type { Conversation } from '@/entities/conversation';

import type { DateGroupListItem } from '../../lib/hooks/conversationListState.types';
import type { DropAreaState } from '../../lib/hooks/useDragAndDrop';
import { LoadMoreSentinel } from './LoadMoreSentinel';

const LOADING_SKELETON_COUNT = 3;

export type RenderConversationItem = (conversation: Conversation, onItemHover: (itemId: string, isHovered: boolean) => void, isNextItemHovered: boolean) => ReactNode;

/**
 * `GroupedConversations.tsx` injects `displayName` (a `DATE_GROUP_DISPLAY_
 * NAMES` lookup) on top of the `DateGroupListItem` it receives from its own
 * caller — see that component's module doc for why `total`/`conversations`
 * live on `DateGroupListItem` (`lib/hooks/conversationListState.types.ts`)
 * rather than on `entities/folder`'s own, wire-shape-only `DateGroup`.
 */
interface DateGroupWithDisplayName extends DateGroupListItem {
  readonly displayName?: string;
}

export interface DateGroupProps {
  readonly group: DateGroupWithDisplayName;
  readonly renderConversationItem: RenderConversationItem;
  readonly enableDragAndDrop?: boolean | undefined;
  readonly getDropAreaState?: ((dropAreaId: string) => DropAreaState) | undefined;
  readonly isExpanded: boolean;
  readonly onToggleExpanded: (groupName: string) => void;
  readonly onLoadMore: () => void;
  readonly isLoadingMore?: boolean | undefined;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/ui/
 * groups/DateGroup.jsx` (unit C2).
 *
 * NOT ported: the baseline's `dropAreaState`/`otherDropState` computation
 * (`DateGroup.jsx:33-41`), guarded there by its own `eslint-disable-next-
 * line no-unused-vars` — it calls `getDropAreaState` and destructures the
 * result into variables that are never applied to any rendered element.
 * Confirmed dead by that suppression comment itself, unlike other baseline
 * quirks ported faithfully elsewhere in this unit. `enableDragAndDrop`/
 * `getDropAreaState` stay in this component's prop type — its caller,
 * `GroupedConversations`, still forwards them — but neither is read in
 * this function body.
 *
 * The header is a real `ButtonBase`, not the baseline's `Box` + bare
 * `onClick` — jsx-a11y's `click-events-have-key-events`/`no-static-
 * element-interactions` are hard lint errors in this repo (no equivalent
 * gate in the old app), and `shared/ui/CategoryItemCard`'s port already
 * established swapping a non-interactive clickable `Box` for a real
 * interactive element as this codebase's fix for that exact defect class.
 * The chevron here is a plain icon, not the baseline's nested `IconButton`
 * — nesting a `<button>` inside this row's own `<button>` (what `ButtonBase`
 * renders) would be invalid HTML once the row itself is the interactive
 * element; the whole row was always the click target in the baseline too
 * (the `onClick` sat on the outer `Box`, not the `IconButton`). `aria-
 * expanded` is likewise new (the baseline's plain `Box` had none) — a real
 * disclosure-widget trigger should expose its expanded state, and it is
 * free now that the trigger is a real interactive element.
 */
export function DateGroup({ group, renderConversationItem, isExpanded, onToggleExpanded, onLoadMore, isLoadingMore = false }: DateGroupProps): ReactNode {
  const [hoveredItemId, setHoveredItemId] = useState<string | null>(null);

  const handleItemHover = useCallback((itemId: string, isHovered: boolean): void => {
    setHoveredItemId(isHovered ? itemId : null);
  }, []);

  const handleToggleExpanded = useCallback((): void => {
    onToggleExpanded(group.name);
  }, [group.name, onToggleExpanded]);

  return (
    <Box sx={(theme: Theme) => ({ marginBottom: theme.spacing(1) })}>
      <ButtonBase
        onClick={handleToggleExpanded}
        aria-expanded={isExpanded}
        sx={(theme: Theme) => ({
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'flex-start',
          width: '100%',
          gap: theme.spacing(1),
          padding: theme.spacing(1, 0.5),
          marginBottom: theme.spacing(0.5),
          '&:hover': { backgroundColor: theme.vars.palette.action.hover },
        })}
      >
        <Box
          aria-hidden="true"
          data-testid="date-group-chevron"
          sx={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            transform: isExpanded ? 'rotate(90deg)' : 'rotate(0deg)',
            transition: 'transform 0.15s',
          }}
        >
          <ArrowForwardIosSharpIcon
            fontSize="small"
            sx={(theme: Theme) => ({ color: theme.vars.palette.icon.fill.secondary })}
          />
        </Box>
        {/*
          * `component="span"`, not MUI's default mapping for `subtitle2`
          * (`<h6>`). This text is the accessible LABEL of the surrounding
          * `ButtonBase`, not a section heading, and rendering it as an `<h6>`
          * beside the folder accordions' `<h3>` summaries produced a real
          * `heading-order` violation (axe, moderate) the moment this list was
          * first mounted on a route. Presentation is unchanged — `variant`
          * still drives the typography.
          */}
        <Typography
          component="span"
          variant="subtitle2"
          // `text.default` (#A9B7C1 in dark), not `text.secondary` — this
          // pack defines `text.secondary` as pure #FFFFFF, so the group
          // heading rendered brighter than the conversation names under it.
          // The production rail's own "Older"/"Today" heading measures
          // `rgb(169, 183, 193)`.
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.default, textTransform: 'none' })}
        >
          {group.displayName ?? group.name}
        </Typography>
      </ButtonBase>

      <Collapse in={isExpanded}>
        <Box sx={(theme: Theme) => ({ paddingLeft: theme.spacing(2) })}>
          {group.conversations.map((conversation, index) => {
            const nextConversation = group.conversations[index + 1];
            const isNextItemHovered = nextConversation?.id === hoveredItemId;
            return renderConversationItem(conversation, handleItemHover, isNextItemHovered);
          })}

          {isLoadingMore &&
            Array.from({ length: LOADING_SKELETON_COUNT }).map((_, index) => (
              <Skeleton
                key={`skeleton-${index}`}
                data-testid="date-group-skeleton"
                animation="wave"
                variant="rectangular"
                width="100%"
                height="2.5rem"
                sx={(theme: Theme) => ({ borderRadius: theme.vars.shape.radiusSm, marginBottom: theme.spacing(0.25) })}
              />
            ))}

          <LoadMoreSentinel
            listCurrentSize={group.conversations.length}
            totalAvailableCount={group.total ?? 0}
            onLoadMore={onLoadMore}
            isLoading={isLoadingMore}
          />
        </Box>
      </Collapse>
    </Box>
  );
}

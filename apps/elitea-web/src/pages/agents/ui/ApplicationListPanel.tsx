import { useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { EntityCardList, EntityEmptyState, entityTypeIcon, type EntityListItem } from '@/shared/ui/EntityCardList';

import { AgentCardAvatar } from './AgentCardAvatar';

/**
 * The card grid for `Latest`/`MyLiked`/`Trending`/`PrivateAgentsList` —
 * the port of the baseline's `components/CardList.jsx` for these four
 * pages, now that the grid itself exists as `shared/ui/EntityCardList`.
 *
 * This file used to render a plain MUI `<List>` of `name`/`description`
 * rows, a disclosed scope reduction from the era when no card grid was
 * ported anywhere. It is a grid of real entity cards now: round gradient
 * icon tile, two-line title, and a bottom row of author avatars and tag
 * chips, matching `apps/elitea-ui/src/components/Card.jsx` and the
 * production reference. `?view=table` switches the same rows to the table
 * rendering; the grid decides that from the search param itself.
 *
 * A page-owned component (`pages/agents/ui/`), not `features/` or
 * `entities/` — it holds no fetching or domain logic, purely list layout +
 * empty/loading/error states, matching spec §3.3's "pages/ = layout + slot
 * composition" rule.
 */
export interface ApplicationListRow {
  readonly id: string;
  readonly name: string;
  readonly description: string;
  readonly authors?: readonly {
    readonly id?: string;
    readonly name: string;
    readonly avatar?: string;
    readonly deleted?: boolean;
  }[];
  readonly tags?: readonly string[];
  readonly createdAt?: string;
  /** When the agent last changed: the rich card's muted line. */
  readonly updatedAt?: string;
  /** The persona role (`soul.role`, #3133): the rich card's second line. */
  readonly personaRole?: string;
  /** The version's avatar pack (#3133): the rich card plays it through the live-avatar renderer. */
  readonly avatarPack?: object;
  /** The status as a word (Draft, Published): the card's badge and the table's Status column. */
  readonly badge?: string;
  /** #1995 — the uploaded icon (`meta.icon_meta.url`); the agent glyph when absent. */
  readonly iconUrl?: string;
  /**
   * #915 — set by the caller (which already holds the router's `navigate`)
   * when this row is a fork, to render a "Forked from" link that opens the
   * SOURCE entity. A callback rather than a bare id, same as `EntityListItem
   * .onClick`/`forkedFrom` themselves — keeps this component's own prop
   * count under the §3.5 12-prop budget, which a second top-level
   * `onSelectForkedFrom` callback prop would have breached.
   */
  readonly forkedFrom?: { readonly onClick: () => void };
  /** The row's extra action (the Undo of an automatic hire), in the table view. */
  readonly action?: { readonly label: string; readonly onClick: () => void };
}

export interface ApplicationListPanelProps {
  readonly rows: readonly ApplicationListRow[];
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly errorMessage: string;
  readonly emptyTitle: string;
  readonly emptyDescription: string;
  readonly onSelect: (id: string) => void;
  readonly hasMore: boolean;
  readonly isLoadingMore: boolean;
  readonly onLoadMore: () => void;
  /** Wired to the empty state's `+ Create` CTA when the caller can route to a create page. */
  readonly onCreate?: () => void;
  /** The Agents card of issue 3133 instead of the compact tile, with its Chat action when the page can start one. */
  readonly rich?: RichCardOptions;
}

/** The grid's two cards (`EntityCardList`'s `variant`). */
const COMPACT_CARD = 'compact';
const RICH_CARD = 'rich';

/** What the rich card adds: the Chat action, and which agent's chat is starting (every Chat waits for it). */
export interface RichCardOptions {
  readonly onChat?: (row: ApplicationListRow) => void;
  readonly chattingId?: string | undefined;
}

/** The compact tile's icon: the uploaded icon, or the agent glyph. */
function compactIcon(row: ApplicationListRow): ReactNode {
  return row.iconUrl === undefined ? (
    entityTypeIcon('agent')
  ) : (
    <Box component="img" src={row.iconUrl} alt="" data-testid="application-card-icon" sx={iconImageSx} />
  );
}

/** The rich card's own fields: the live avatar (none means the card's monogram), the persona, the last change and Chat. */
function richFields(row: ApplicationListRow, rich: RichCardOptions): Partial<EntityListItem> {
  const hasAvatar = row.iconUrl !== undefined || row.avatarPack !== undefined;
  const onChat = rich.onChat;
  return {
    icon: hasAvatar ? <AgentCardAvatar iconUrl={row.iconUrl} pack={row.avatarPack} /> : undefined,
    ...(row.personaRole === undefined ? {} : { subtitle: row.personaRole }),
    ...(row.updatedAt === undefined ? {} : { updatedAt: row.updatedAt }),
    ...(onChat === undefined
      ? {}
      : { primaryAction: { label: t('pages.agents.list.chat', 'Chat'), onClick: () => onChat(row), disabled: rich.chattingId !== undefined } }),
  };
}

function toListItem(row: ApplicationListRow, onSelect: (id: string) => void, rich: RichCardOptions | undefined): EntityListItem {
  return {
    id: row.id,
    name: row.name,
    description: row.description,
    icon: compactIcon(row),
    authors: row.authors ?? [],
    tags: (row.tags ?? []).map((tag) => ({ id: tag, name: tag })),
    ...(row.createdAt === undefined ? {} : { createdAt: row.createdAt }),
    ...(row.badge === undefined ? {} : { badge: row.badge }),
    onClick: () => {
      onSelect(row.id);
    },
    ...(row.forkedFrom === undefined ? {} : { forkedFrom: row.forkedFrom }),
    ...(row.action === undefined ? {} : { action: row.action }),
    ...(rich === undefined ? {} : richFields(row, rich)),
  };
}

export function ApplicationListPanel({
  rows,
  isLoading,
  isError,
  errorMessage,
  emptyTitle,
  emptyDescription,
  onSelect,
  hasMore,
  isLoadingMore,
  onLoadMore,
  onCreate,
  rich,
}: ApplicationListPanelProps): ReactNode {
  const items = useMemo<EntityListItem[]>(() => rows.map((row) => toListItem(row, onSelect, rich)), [rows, onSelect, rich]);

  return (
    <>
      <EntityCardList
        items={items}
        variant={rich === undefined ? COMPACT_CARD : RICH_CARD}
        isLoading={isLoading}
        isError={isError}
        errorMessage={errorMessage}
        emptyState={
          <EntityEmptyState
            title={emptyTitle}
            description={emptyDescription}
            art="applications"
            {...(onCreate === undefined ? {} : { onCreateClick: onCreate })}
          />
        }
      />
      {hasMore && (
        <Box sx={loadMoreSx}>
          <BaseBtn variant="secondary" disabled={isLoadingMore} onClick={onLoadMore}>
            {t('pages.agents.list.loadMore', 'Load more')}
          </BaseBtn>
        </Box>
      )}
    </>
  );
}

const iconImageSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  height: '100%',
  objectFit: 'cover',
  borderRadius: theme.vars.shape.radiusPill,
});

const loadMoreSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  justifyContent: 'center',
  padding: theme.spacing(2),
});

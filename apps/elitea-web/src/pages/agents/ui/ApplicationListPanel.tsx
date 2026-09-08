import { useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { EntityCardList, EntityEmptyState, entityTypeIcon, type EntityListItem } from '@/shared/ui/EntityCardList';

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
  readonly authors?: readonly { readonly id?: string; readonly name: string; readonly avatar?: string }[];
  readonly tags?: readonly string[];
  readonly createdAt?: string;
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
  readonly railVisible?: boolean;
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
  railVisible = true,
}: ApplicationListPanelProps): ReactNode {
  const items = useMemo<EntityListItem[]>(
    () =>
      rows.map((row) => ({
        id: row.id,
        name: row.name,
        description: row.description,
        icon: entityTypeIcon('agent'),
        authors: row.authors ?? [],
        tags: (row.tags ?? []).map((tag) => ({ id: tag, name: tag })),
        ...(row.createdAt === undefined ? {} : { createdAt: row.createdAt }),
        onClick: () => {
          onSelect(row.id);
        },
      })),
    [rows, onSelect],
  );

  return (
    <>
      <EntityCardList
        items={items}
        isLoading={isLoading}
        isError={isError}
        errorMessage={errorMessage}
        railVisible={railVisible}
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
          <BaseBtn
            variant="secondary"
            disabled={isLoadingMore}
            onClick={onLoadMore}
          >
            {t('pages.agents.list.loadMore', 'Load more')}
          </BaseBtn>
        </Box>
      )}
    </>
  );
}

const loadMoreSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', justifyContent: 'center', padding: theme.spacing(2) });

import { useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { EntityCardList, EntityEmptyState, entityTypeIcon, type EntityListItem } from '@/shared/ui/EntityCardList';

/**
 * The card grid for `Latest`/`MyLiked`/`Trending`/`PrivatePipelinesList` —
 * the port of the baseline's `components/CardList.jsx` for these four pages,
 * now that the grid itself exists as `shared/ui/EntityCardList`.
 *
 * This file used to render a plain MUI `<List>` of `name`/`description`
 * rows, a disclosed scope reduction from the era when no card grid was
 * ported anywhere. It is a grid of real entity cards now — the pipeline
 * flow glyph in a round gradient tile, a two-line title, and a bottom row
 * of author avatars and divider-separated tag chips, matching
 * `apps/elitea-ui/src/components/Card.jsx` and the production reference.
 *
 * A page-owned component (`pages/pipelines/ui/`) and still deliberately NOT
 * an import of `pages/agents/ui/ApplicationListPanel.tsx`: the two page
 * slices land and evolve independently. What they now share is the real
 * shared surface underneath (`shared/ui/EntityCardList`), which is where
 * the duplication actually belonged.
 */
export interface PipelineListRow {
  readonly id: string;
  readonly name: string;
  readonly description: string;
  readonly authors?: readonly { readonly id?: string; readonly name: string; readonly avatar?: string }[];
  readonly tags?: readonly string[];
  readonly createdAt?: string;
}

export interface PipelineListPanelProps {
  readonly rows: readonly PipelineListRow[];
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
}

export function PipelineListPanel({
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
}: PipelineListPanelProps): ReactNode {
  const items = useMemo<EntityListItem[]>(
    () =>
      rows.map((row) => ({
        id: row.id,
        name: row.name,
        description: row.description,
        icon: entityTypeIcon('pipeline'),
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
            {t('pages.pipelines.list.loadMore', 'Load more')}
          </BaseBtn>
        </Box>
      )}
    </>
  );
}

const loadMoreSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', justifyContent: 'center', padding: theme.spacing(2) });

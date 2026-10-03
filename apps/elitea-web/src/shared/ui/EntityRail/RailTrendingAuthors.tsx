import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import List from '@mui/material/List';
import ListItem from '@mui/material/ListItem';
import ListItemAvatar from '@mui/material/ListItemAvatar';
import ListItemText from '@mui/material/ListItemText';
import Skeleton from '@mui/material/Skeleton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetSocialTrendingAuthors } from '@/shared/api/generated/social/social';
import { unwrapList } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/**
 * "Trending Authors" — the rail's alternative to the author card on the
 * public feeds (`latest`/`my-liked`/`trending`) and on the agents/pipelines
 * Admin tab (baseline: `components/TeamMates.jsx`, selected by
 * `components/RightInfoPanel.jsx:44-48`, and rendered on the Admin tab by
 * `pages/Applications/PrivateAgentsList.jsx:141-151`).
 *
 * Backed by `GET /social/trending_authors/prompt_lib/{projectId}`
 * (`useGetSocialTrendingAuthors`) — the REAL one that ranks authors by like
 * count, not the `/elitea_core/trending_authors` eliteacore stub that always
 * returns `[]` (both exist; the generated client's own doc comment at
 * `social.ts:1198-1206` spells out the difference).
 *
 * **Disclosed narrowing:** the rows are not clickable. The baseline's
 * `TeamMates` navigates to `/user-public/{tab}?author_id=…&author_name=…`
 * via `hooks/useCardNavigate.js`, which is a router concern the `shared/`
 * layer may not reach (R-L1: no `pages/` import, and this app's typed
 * `navigate({to})` calls are checked against the real route tree). `onSelect`
 * is an optional prop so a page-layer caller can supply that navigation;
 * without it the list renders as plain, non-interactive rows rather than as
 * buttons that do nothing.
 */
export interface RailTrendingAuthor {
  readonly id: string;
  readonly name: string;
  readonly email: string;
  readonly avatar?: string;
  readonly likes?: number;
}

export interface RailTrendingAuthorsViewProps {
  readonly authors: readonly RailTrendingAuthor[];
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly onSelect?: (author: RailTrendingAuthor) => void;
}

const SKELETON_ROW_COUNT = 3;

/** Presentational half — no data source, so a test can drive every state directly. */
export function RailTrendingAuthorsView({ authors, isLoading, isError, onSelect }: RailTrendingAuthorsViewProps): ReactNode {
  return (
    <Box data-testid="entity-rail-trending-authors">
      <Typography
        component="div"
        variant="headingSmall"
        sx={titleSx}
      >
        {t('shared.ui.entityRail.trendingAuthors.title', 'Trending Authors')}
      </Typography>
      {isLoading &&
        Array.from({ length: SKELETON_ROW_COUNT }).map((_unused, index) => (
          <Skeleton
            key={`trending-author-skeleton-${String(index)}`}
            variant="rectangular"
            data-testid="entity-rail-trending-author-skeleton"
            sx={skeletonSx}
          />
        ))}
      {isError && !isLoading && <Typography variant="bodyMedium" component="p">{t('shared.ui.entityRail.trendingAuthors.loadError', 'Failed to load.')}</Typography>}
      {!isLoading && !isError && authors.length === 0 && (
        <Typography variant="bodyMedium" component="p">{t('shared.ui.entityRail.trendingAuthors.empty', 'No authors to display.')}</Typography>
      )}
      {!isLoading && !isError && authors.length > 0 && (
        <List dense>
          {authors.map((author) => (
            <ListItem
              key={author.id}
              disableGutters
              data-testid="entity-rail-trending-author"
              {...(onSelect === undefined
                ? {}
                : {
                    onClick: () => {
                      onSelect(author);
                    },
                  })}
            >
              <ListItemAvatar>
                <Avatar
                  alt={author.name}
                  {...(author.avatar === undefined || author.avatar === '' ? {} : { src: author.avatar })}
                  sx={avatarSx}
                >
                  {author.name.charAt(0).toUpperCase()}
                </Avatar>
              </ListItemAvatar>
              <ListItemText
                primary={author.name}
                secondary={author.email}
              />
            </ListItem>
          ))}
        </List>
      )}
    </Box>
  );
}

export interface RailTrendingAuthorsProps {
  /** `undefined` while the selected project is not resolved yet — the query stays disabled and the panel renders its loading rows. */
  readonly projectId: string | undefined;
  readonly onSelect?: (author: RailTrendingAuthor) => void;
}

/** `RailTrendingAuthorsView` wired to the real, already-generated social client. */
export function RailTrendingAuthors({ projectId, onSelect }: RailTrendingAuthorsProps): ReactNode {
  const query = useGetSocialTrendingAuthors(projectId ?? '', { query: { enabled: projectId !== undefined } });
  const authors = useMemo(() => unwrapList<RailTrendingAuthor>(query.data, 'RailTrendingAuthors/socialTrendingAuthors'), [query.data]);

  return (
    <RailTrendingAuthorsView
      authors={authors}
      isLoading={projectId === undefined || (query.isFetching && query.data === undefined)}
      isError={query.isError}
      {...(onSelect === undefined ? {} : { onSelect })}
    />
  );
}

const titleSx: SxProps<Theme> = (theme: Theme) => ({ marginBottom: theme.spacing(1) });

const avatarSx: SxProps<Theme> = { width: '2rem', height: '2rem' };

const skeletonSx: SxProps<Theme> = (theme: Theme) => ({
  marginBottom: theme.spacing(1),
  width: '100%',
  height: '2.5rem',
  borderRadius: theme.vars.shape.radiusMd,
});

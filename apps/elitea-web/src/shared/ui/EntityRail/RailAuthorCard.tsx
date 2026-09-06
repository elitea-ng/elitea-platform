import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import Skeleton from '@mui/material/Skeleton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetAuthorDetail } from '@/shared/api/generated/applications/applications';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

import {
  railStatForKind,
  railStatForPath,
  resolveRailStat,
  type RailAuthorCounts,
  type RailStatKind,
  type RailStatValues,
} from './lib/railStatistics';

/**
 * The rail's author card — ported from `apps/elitea-ui/src/[fsd]/entities/
 * author/ui/AuthorInformation.jsx` (card geometry at :91-99: `1px solid
 * border.table`, `padding 1rem 0.75rem`, `borderRadius .5rem`, `width
 * 19.5rem`, `background background.tabPanel`; 45px avatar at :60-64;
 * route-keyed statistic at :41-53, which lives in `./lib/railStatistics.ts`).
 *
 * WHEN it renders instead of `RailTrendingAuthors` is the CALLER's decision,
 * exactly as it is in the baseline (`components/RightInfoPanel.jsx:25-33`:
 * an author is in scope, or the selected project is the user's personal
 * project) — that predicate needs the router's `auth` context and the
 * `author_id` search param, both of which belong to the page layer.
 * `shouldShowAuthorCard` below is that predicate as a pure function so a
 * page does not re-derive it.
 *
 * **Disclosed narrowings:**
 *  - **No avatar image.** The baseline's `UserAvatar` renders
 *    `authorDetails.avatar`, which the Go handler hardcodes (see
 *    `authorDetail.zod.ts`'s own field notes) — MUI's `Avatar` with the
 *    name's initials is used instead, and `avatar` is passed through as the
 *    `src` when the server does send one.
 *  - **`public_applications` is a hardcoded 0 server-side**
 *    (`authorDetail.zod.ts`: "Always 0 — hardcoded stub, handler.go:433").
 *    The `/agents` "Published" row is therefore wired to a real field that
 *    currently always reads 0 — faithful to the baseline, which renders the
 *    same zero.
 */
export interface RailAuthorCardViewProps {
  readonly name: string;
  readonly avatar?: string;
  readonly statistic?: RailStatValues;
  /** The optional `Indexes: N` row the baseline's toolkits caller passes (`AuthorInformation.jsx`'s `indexesTotal`). */
  readonly indexesTotal?: number;
  readonly isLoading: boolean;
}

/**
 * `RightInfoPanel.jsx:25-33` — the author card wins over trending authors
 * when an author is IN SCOPE, or when the selection IS the user's own
 * personal project.
 *
 * `authorIdFromUrl` is deliberately the URL `author_id` param, NOT the
 * resolved author the card then fetches: the baseline's own predicate reads
 * `useAuthorIdFromUrl()` (which is `undefined` on an ordinary list page),
 * while the FETCH falls back to the current user's id
 * (`useQueryTrendingAuthor.js:18`). Passing the resolved id here would make
 * the first branch always true and trending authors unreachable.
 */
export function shouldShowAuthorCard(authorIdFromUrl: string | undefined, selectedProjectId: string | undefined, personalProjectId: string | undefined): boolean {
  if (authorIdFromUrl !== undefined && authorIdFromUrl !== '') return true;
  if (selectedProjectId !== undefined && personalProjectId !== undefined) return selectedProjectId === personalProjectId;
  return true;
}

function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter((part) => part !== '');
  return parts
    .slice(0, 2)
    .map((part) => part.charAt(0).toUpperCase())
    .join('');
}

const STAT_LABELS: Readonly<Record<RailStatKind, () => string>> = {
  agents: () => t('shared.ui.entityRail.author.agents', 'Agents'),
  skills: () => t('shared.ui.entityRail.author.skills', 'Skills'),
  pipelines: () => t('shared.ui.entityRail.author.pipelines', 'Pipelines'),
  toolkits: () => t('shared.ui.entityRail.author.toolkits', 'Toolkits'),
};

interface StatPairProps {
  readonly label: string;
  readonly value: number;
  /** Stable, translation-independent hook for tests — the visible label is localised copy. */
  readonly testId: string;
  /** `AuthorStatistics.jsx` separates the second pair from the first with `margin-left: 0.5rem`. */
  readonly gapBefore?: boolean;
}

/**
 * `AuthorStatistics.jsx:16-50` renders the entity total and "Published" as
 * label/value SPAN pairs inside ONE `<Typography variant="bodySmall">` — a
 * single line reading `Agents: 4  Published: 0`, with each VALUE (not the
 * label) painted `text.secondary`. The port emitted a separate block row per
 * statistic, which stacked them vertically and made the card two lines
 * taller than production's.
 */
function StatPair({ label, value, testId, gapBefore = false }: StatPairProps): ReactNode {
  return (
    <Box
      component="span"
      data-testid={testId}
      sx={gapBefore ? statLabelGapSx : undefined}
    >
      {`${label}:`}
      <Box
        component="span"
        sx={statValueSx}
      >
        {value}
      </Box>
    </Box>
  );
}

/** Presentational half — no data source, so a test can drive every state directly. */
export function RailAuthorCardView({ name, avatar, statistic, indexesTotal, isLoading }: RailAuthorCardViewProps): ReactNode {
  if (isLoading) {
    return (
      <Skeleton
        variant="rectangular"
        data-testid="entity-rail-author-skeleton"
        sx={skeletonSx}
      />
    );
  }
  if (name.trim() === '') return null;

  return (
    <Box
      data-testid="entity-rail-author"
      sx={cardSx}
    >
      <Box sx={rowSx}>
        <Avatar
          alt={name}
          {...(avatar === undefined ? {} : { src: avatar })}
          sx={avatarSx}
        >
          {initials(name)}
        </Avatar>
        <Box sx={infoSx}>
          <Typography
            variant="labelMedium"
            component="div"
            sx={nameSx}
          >
            {name}
          </Typography>
          <Box sx={statsBlockSx}>
            {statistic !== undefined && (
              <Typography
                variant="bodySmall"
                component="div"
              >
                <StatPair
                  label={STAT_LABELS[statistic.kind]()}
                  value={statistic.value}
                  testId="entity-rail-author-total"
                />
                {statistic.published !== undefined && (
                  <StatPair
                    label={t('shared.ui.entityRail.author.published', 'Published')}
                    value={statistic.published}
                    testId="entity-rail-author-published"
                    gapBefore
                  />
                )}
              </Typography>
            )}
            {indexesTotal !== undefined && (
              <Typography
                variant="bodySmall"
                component="div"
              >
                <StatPair
                  label={t('shared.ui.entityRail.author.indexes', 'Indexes')}
                  value={indexesTotal}
                  testId="entity-rail-author-indexes"
                />
              </Typography>
            )}
          </Box>
        </Box>
      </Box>
    </Box>
  );
}

export interface RailAuthorCardProps {
  /** `undefined` disables the request (nothing to ask for) and renders nothing. */
  readonly authorId: number | undefined;
  /** The route the statistic line is keyed off (`AuthorInformation.jsx:42` reads `location.pathname`). */
  readonly pathname: string;
  /** Overrides the pathname lookup — for `/user-public/:tab`, whose prefix names no entity while its TAB does. */
  readonly statKind?: RailStatKind;
  readonly indexesTotal?: number;
}

/** `RailAuthorCardView` wired to the real, already-generated `GetAuthorDetail` client. */
export function RailAuthorCard({ authorId, pathname, statKind, indexesTotal }: RailAuthorCardProps): ReactNode {
  const query = useGetAuthorDetail(authorId ?? 0, { query: { enabled: authorId !== undefined } });
  const detail = useMemo(() => (unwrapBody(query.data) ?? {}) as RailAuthorCounts & { name?: string; avatar?: string }, [query.data]);

  const descriptor = statKind === undefined ? railStatForPath(pathname) : railStatForKind(statKind);
  const statistic = descriptor === undefined ? undefined : resolveRailStat(descriptor, detail);

  if (authorId === undefined) return null;

  return (
    <RailAuthorCardView
      name={detail.name ?? ''}
      {...(detail.avatar === undefined ? {} : { avatar: detail.avatar })}
      {...(statistic === undefined ? {} : { statistic })}
      {...(indexesTotal === undefined ? {} : { indexesTotal })}
      isLoading={query.isFetching && query.data === undefined}
    />
  );
}

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  border: `0.0625rem solid ${theme.vars.palette.border.table}`,
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.tabPanel,
  paddingBlock: theme.spacing(2),
  paddingInline: theme.spacing(1.5),
  maxHeight: '50vh',
  // `AuthorInformation.jsx`'s `mainContainer` — a fixed 19.5rem card with a
  // 1rem gutter under it, not a full-bleed rail child.
  width: '19.5rem',
  marginBottom: theme.spacing(2),
  flexShrink: 0,
  boxSizing: 'border-box',
});

/** `AuthorStatistics.jsx`'s `statisticsBlock`. */
const statsBlockSx: SxProps<Theme> = { minHeight: '1.5rem', display: 'flex', flexDirection: 'column', justifyContent: 'center' };

const statValueSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, marginLeft: '0.25rem' });

const statLabelGapSx: SxProps<Theme> = { marginLeft: '0.5rem' };

const rowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'row' };

const avatarSx: SxProps<Theme> = { width: '2.8125rem', height: '2.8125rem' };

const infoSx: SxProps<Theme> = (theme: Theme) => ({
  marginLeft: theme.spacing(2),
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'center',
  minWidth: 0,
});

const nameSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, height: '1.5rem', display: 'flex', alignItems: 'center' });

const skeletonSx: SxProps<Theme> = (theme: Theme) => ({
  marginTop: theme.spacing(1),
  width: '100%',
  height: '3.75rem',
  borderRadius: theme.vars.shape.radiusMd,
});

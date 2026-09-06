import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

// The baseline's `ClearIcon` (`@/components/Icons/ClearIcon`) is not in S2's
// ported `shared/ui/icons/` set — same interim-icon note
// `features/toolkits`' `ToolkitTypesPanel.tsx` already records for the same
// glyph. `Clear` is the standard R-I1-compliant single-icon import.
import ClearIcon from '@mui/icons-material/Clear';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Skeleton from '@mui/material/Skeleton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { unwrapListPage } from '@/shared/api/unwrap';
import { useListTags } from '@/shared/api/generated/tags/tags';
import { t } from '@/shared/i18n';

import { sortTagsSelectedFirst, type RailTag } from './lib/railTags';

/**
 * The rail's "Tags" panel — ported from `apps/elitea-ui/src/components/
 * Categories.jsx` (title at :70, container geometry at :25-32 —
 * `marginBottom: 24px`, `minHeight: 5.5em`, hidden scrollbar — skeleton
 * chips at :44-52/:296-305, the clear-all control at :266-282, and the
 * selected-first sort at :108-124, which lives in `./lib/railTags.ts`).
 *
 * **Disclosed narrowings, all of them backend/state facts rather than
 * choices:**
 *  - **No paging.** `Categories.jsx` re-fetches with an incrementing `page`
 *    on scroll-to-bottom. `ListTags` (`GET /elitea_core/tags/prompt_lib/
 *    {projectId}`, `shared/api/generated/tags/tags.ts`) declares NO request
 *    parameters at all — no `page`, `query`, `statuses`, `entity_coverage`,
 *    `my_liked` or `author_id`. Every one of the baseline's per-route
 *    request branches (`isFromApplications`/`isFromPipelines`/
 *    `isFromSkills`/`isOnUserPublic`) therefore has nothing to send, and the
 *    handler returns the project's full `{rows, total}` tag list in one
 *    response (`internal/api/v2/tags/handler.go:37-49`). Scroll paging over
 *    a complete list would be a no-op, so it is not reproduced.
 *  - **No `tagsOnVisibleCards` cache.** The baseline resolves a selected tag
 *    NAME that is missing from the fetched page against a second redux
 *    cache derived from the cards on screen. This app has no such cache;
 *    `sortTagsSelectedFirst`'s `extraTags` argument is the seam for one, and
 *    without it an unresolvable selected name is dropped from the chip row
 *    exactly as the baseline's own `.filter(tag => tag)` drops it.
 */
export interface RailTagsPanelViewProps {
  readonly tags: readonly RailTag[];
  readonly selectedTags: readonly string[];
  readonly onToggleTag: (name: string) => void;
  readonly onClearTags: () => void;
  readonly isLoading: boolean;
  readonly isError: boolean;
  /** @default t('shared.ui.entityRail.tags.title', 'Tags') */
  readonly title?: string;
}

const SKELETON_CHIP_COUNT = 10;

/** Presentational half — no data source, so a test can drive every state directly. */
export function RailTagsPanelView({ tags, selectedTags, onToggleTag, onClearTags, isLoading, isError, title }: RailTagsPanelViewProps): ReactNode {
  const heading = title ?? t('shared.ui.entityRail.tags.title', 'Tags');
  const clearLabel = t('shared.ui.entityRail.tags.clearAll', 'Clear all');
  const sorted = useMemo(() => sortTagsSelectedFirst(tags, selectedTags), [tags, selectedTags]);
  const handleClick = useCallback(
    (name: string) => () => {
      onToggleTag(name);
    },
    [onToggleTag],
  );

  return (
    <Box
      data-testid="entity-rail-tags"
      sx={panelSxFor(isLoading, isError, sorted.length)}
    >
      <Box sx={headerRowSx}>
        <Typography
          component="div"
          variant="subtitle"
          sx={titleSx}
        >
          {heading}
        </Typography>
        {selectedTags.length > 0 && (
          <Tooltip
            title={clearLabel}
            placement="top"
          >
            <IconButton
              color="secondary"
              onClick={onClearTags}
              aria-label={clearLabel}
              data-testid="tags-panel-clear-all"
            >
              <ClearIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        )}
      </Box>
      <Box sx={chipsContainerSx}>
        {isLoading &&
          Array.from({ length: SKELETON_CHIP_COUNT }).map((_unused, index) => (
            <Skeleton
              // eslint-disable-next-line react/no-array-index-key -- fixed-length placeholder row with no identity of its own; the baseline keys it by index too (Categories.jsx:299).
              key={`tag-skeleton-${String(index)}`}
              variant="rectangular"
              data-testid="entity-rail-tag-skeleton"
              sx={skeletonChipSx}
            />
          ))}
        {isError && !isLoading && <Typography variant="body2">{t('shared.ui.entityRail.tags.loadError', 'Failed to load.')}</Typography>}
        {!isLoading && !isError && sorted.length === 0 && (
          <Typography variant="body2">{t('shared.ui.entityRail.tags.empty', 'No tags to display.')}</Typography>
        )}
        {!isLoading &&
          !isError &&
          sorted.map((tag) => (
            <Chip
              key={tag.id}
              label={tag.name}
              clickable
              aria-pressed={selectedTags.includes(tag.name)}
              onClick={handleClick(tag.name)}
              data-testid={`tags-panel-chip-${tag.name}`}
              sx={selectedTags.includes(tag.name) ? selectedChipSx : chipSx}
            />
          ))}
      </Box>
    </Box>
  );
}

export interface RailTagsPanelProps extends Omit<RailTagsPanelViewProps, 'tags' | 'isLoading' | 'isError'> {
  /** `undefined` while the selected project is not resolved yet — the query stays disabled and the panel renders its loading row. */
  readonly projectId: string | undefined;
}

/** `RailTagsPanelView` wired to the real, already-generated `ListTags` client. */
export function RailTagsPanel({ projectId, ...viewProps }: RailTagsPanelProps): ReactNode {
  const query = useListTags(projectId ?? '', { query: { enabled: projectId !== undefined } });
  const tags = useMemo(() => unwrapListPage<RailTag>(query.data, 'RailTagsPanel/listTags').rows, [query.data]);

  return (
    <RailTagsPanelView
      {...viewProps}
      tags={tags}
      isLoading={projectId === undefined || (query.isFetching && query.data === undefined)}
      isError={query.isError}
    />
  );
}

/**
 * `RightInfoPanel.jsx` gives its column `height: 100dvh` and passes
 * `Categories` `{flex: 1, minHeight: 0}` — the tag list is what ABSORBS the
 * rail's free height, which is what leaves the author card sitting on the
 * bottom edge of the viewport in production when the tag list is long.
 */
const panelSx: SxProps<Theme> = { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' };

/**
 * The same panel when it has NO chips to hold.
 *
 * DEFECT this repairs. `flex: 1` was unconditional, so a project with no tags
 * still handed the whole rail to a panel holding one line of text
 * ("No tags to display."), and the author card was pushed onto the bottom edge
 * of the viewport with ~600px of dead space above it — on every list page of
 * a fresh deployment, which is every list page a new client first opens.
 * Absorbing the slack is what production does with a FULL tag list; an empty
 * one has nothing to absorb it with, and the card belongs under the panel.
 *
 * The loading state keeps `panelSx`: it renders ten skeleton chips, so it has
 * a list to hold, and shrinking there would move the card twice.
 */
const compactPanelSx: SxProps<Theme> = { flex: '0 0 auto', minHeight: 0, display: 'flex', flexDirection: 'column' };

/** The panel absorbs the rail's free height only when it has a list to hold. */
function panelSxFor(isLoading: boolean, isError: boolean, chipCount: number): SxProps<Theme> {
  if (isLoading) return panelSx;
  return !isError && chipCount > 0 ? panelSx : compactPanelSx;
}

const headerRowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'row',
  justifyContent: 'space-between',
  alignItems: 'center',
  paddingRight: theme.spacing(2),
});

const titleSx: SxProps<Theme> = (theme: Theme) => ({ marginBottom: theme.spacing(1), marginRight: theme.spacing(2) });

const chipsContainerSx: SxProps<Theme> = (theme: Theme) => ({
  marginBottom: theme.spacing(3),
  minHeight: '5.5em',
  flex: 1,
  display: 'flex',
  flexWrap: 'wrap',
  gap: theme.spacing(1),
  alignContent: 'flex-start',
  overflowY: 'auto',
  scrollbarWidth: 'none',
  '&::-webkit-scrollbar': { display: 'none' },
});

const chipSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  border: 'none',
  backgroundColor: theme.vars.palette.background.tag.default,
  color: theme.vars.palette.text.tag.default,
});

const selectedChipSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  border: 'none',
  backgroundColor: theme.vars.palette.background.tag.selected,
  color: theme.vars.palette.text.tag.selected,
});

const skeletonChipSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  width: '6.25rem',
  height: '2rem',
});

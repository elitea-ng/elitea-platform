import type { ReactNode } from 'react';
import { useCallback } from 'react';

// [A4e] Interim icon, same class of gap `BaseModal.tsx`/`ControlsDropdown.tsx`
// document: the baseline's `ClearIcon` (`@/components/Icons/ClearIcon`) is
// not part of S2's ported `shared/ui/icons/` set (verified by directory
// listing — no `clear-icon.tsx` exists there). `Clear` is the standard
// `@mui/icons-material` glyph (R-I1-compliant single-icon import).
import ClearIcon from '@mui/icons-material/Clear';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/list/
 * ToolkitTypesPanel.jsx` (Wave-2 unit A4e) — the right-panel "Types" filter
 * shown alongside `ToolkitsList`.
 *
 * DISCLOSED REDESIGN, two changes:
 *
 *  1. **The baseline's underlying `Categories` component
 *     (`apps/elitea-ui/src/components/Categories.jsx`, 360 lines) is not
 *     ported.** It is not one of this sub-unit's owned files (only
 *     `ToolkitTypesPanel.jsx` is — `Categories` is a shared, unsliced
 *     `components/` widget with no promoted `entities/`/`shared/` home,
 *     same "no home, build a local scoped copy" situation
 *     `features/agents/ui/AuthorsButton.tsx`'s own doc comment already
 *     documents for its own dependency). What IS ported here is scoped to
 *     exactly this call site's real, OBSERVABLE behaviour: `ToolkitsList`
 *     always passes `customSelectedItems`/`customHandleClick`/
 *     `customHandleClear` (so `Categories`' OWN internal `useTags` branch
 *     never runs) and `maintainAlphabeticalOrder=true` (so its
 *     selected-tags-first re-sort branch never runs either). Traced against
 *     `Categories.jsx`'s own body: with both of those true, its internal
 *     `useLazyTagListQuery` fetch (page/`isFromApplications`/
 *     `isFromPipelines`/`isFromSkills`/`isOnUserPublic` branch — NONE of
 *     which match the toolkits-list route) still runs, but its RESULT is
 *     never rendered (`sortedTagList` reads the `tagList` PROP, not that
 *     query's data) — only its `isLoading`/`isSuccess`/`isError` flags gate
 *     the chip list. For this call site specifically that fetch is
 *     therefore a side-effecting, always-discarded duplicate of the SAME
 *     data `ToolkitsList` already has from `useLoadToolkits`'s own
 *     `tagList` — dropped here rather than faithfully reproduced as
 *     genuinely dead network weight. The panel renders `tagList` directly,
 *     with no independent load/success/error state of its own (the
 *     caller's own `useLoadToolkits` loading state already covers it).
 *  2. **No `useTypes()` hook.** `apps/elitea-ui/src/hooks/toolkit/
 *     useTypes.js` reads/writes the `tags[]` URL param via
 *     `react-router-dom`'s `useLocation`/`useNavigate` (not a dependency of
 *     this app — R1 replaced it with TanStack Router). `selectedTypes`/
 *     `onSelectType`/`onClear` are explicit props instead — the SAME
 *     "page/route-layer caller reads its own `validateSearch`-typed search
 *     params and passes them down" convention `../../lib/hooks/
 *     useLoadToolkits.ts`'s own doc comment (point 3) already establishes
 *     for this exact domain; `'tags[]'` is already registered in
 *     `src/routes/-search/params.ts`'s `paramSchemas` for a future page-
 *     layer caller to read.
 */
export interface ToolkitTypeTag {
  readonly id: string | number;
  readonly name: string;
}

export interface ToolkitTypesPanelProps {
  readonly tagList: readonly ToolkitTypeTag[];
  readonly title?: string;
  readonly selectedTypes: readonly string[];
  readonly onSelectType: (typeName: string) => void;
  readonly onClear: () => void;
  readonly sx?: SxProps<Theme>;
}

export function ToolkitTypesPanel({ tagList, title = 'Types', selectedTypes, onSelectType, onClear, sx }: ToolkitTypesPanelProps): ReactNode {
  const handleClick = useCallback(
    (typeName: string) => () => {
      onSelectType(typeName);
    },
    [onSelectType],
  );

  const showClearButton = selectedTypes.length > 0;

  return (
    <Box sx={sx}>
      <Box sx={headerRowSx}>
        <Typography
          component="div"
          variant="headingSmall"
          sx={titleSx}
        >
          {title}
        </Typography>
        {showClearButton && (
          <Tooltip
            title={t('features.toolkits.toolkitTypesPanel.clearAll', 'Clear all')}
            placement="top"
          >
            <IconButton
              color="secondary"
              onClick={onClear}
              aria-label={t('features.toolkits.toolkitTypesPanel.clearAll', 'Clear all')}
            >
              <ClearIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        )}
      </Box>
      <Box sx={chipsContainerSx}>
        {tagList.length > 0 ? (
          tagList.map((tag) => (
            <Chip
              key={tag.id}
              label={tag.name}
              clickable
              aria-pressed={selectedTypes.includes(tag.name)}
              onClick={handleClick(tag.name)}
              sx={selectedTypes.includes(tag.name) ? selectedChipSx : chipSx}
            />
          ))
        ) : (
          <Typography variant="bodyMedium" component="p">
            {t('features.toolkits.toolkitTypesPanel.empty', 'No {{title}} to display.', { title: title.toLowerCase() })}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

const headerRowSx: SxProps<Theme> = {
  display: 'flex',
  flexWrap: 'wrap',
  flexDirection: 'row',
  justifyContent: 'space-between',
  paddingRight: '1rem',
};

const titleSx: SxProps<Theme> = (theme: Theme) => ({ marginBottom: theme.spacing(1), marginRight: theme.spacing(2) });

const chipsContainerSx: SxProps<Theme> = (theme: Theme) => ({
  marginBottom: theme.spacing(3),
  minHeight: '5.5em',
  overflowY: 'auto',
  display: 'flex',
  flexWrap: 'wrap',
  gap: theme.spacing(1),
  alignContent: 'flex-start',
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

import type { ChangeEvent, ReactNode } from 'react';
import { useCallback } from 'react';

// [S1-D] Interim icon, same class of gap BaseModal.tsx documents for
// `CloseIcon`: the baseline's `SearchIcon` (`@/components/Icons/SearchIcon`)
// is not part of S2's ported `shared/ui/icons/` set (only
// `inventory-search-icon`/`web-search-icon` exist there, neither matching
// this glyph — verified by directory listing). `Search` is the standard
// @mui/icons-material glyph (R-I1-compliant single-icon import).
// TODO(S2 follow-up): swap for `@/shared/ui/icons/search-icon` once it lands.
import SearchIcon from '@mui/icons-material/Search';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { combineSx } from '../lib/combineSx';
import { t } from '@/shared/i18n';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface CategoryFilterProps {
  title?: string;
  searchPlaceholder?: string;
  searchQuery?: string;
  onSearchChange?: (event: ChangeEvent<HTMLInputElement>) => void;
  /** Chip row is shown only when there is more than one category (matches the baseline). */
  allCategories?: string[];
  selectedCategories?: string[];
  onSelectCategory?: (category: string) => void;
  children?: ReactNode;
  categoryListSx?: SxProps<Theme>;
}

/**
 * A search box plus a row of togglable category chips, wrapping a scrollable
 * content area. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/filter/CategoryFilter.jsx`.
 *
 * Deviations (R-C1 fixes, not in the baseline):
 *  - The search `TextField` gets an `aria-label` (falling back to a
 *    translated default) — the baseline relies on `placeholder` alone, which
 *    axe's `color-contrast`-adjacent "form field must have an accessible
 *    name" family of checks correctly rejects (placeholder text is not an
 *    accessible name).
 *  - Category chips are real toggle buttons: `aria-pressed` reflects
 *    selection state, which the baseline (a bare clickable `Chip`, no
 *    pressed-state semantics) did not expose to assistive tech.
 */
export function CategoryFilter({
  title,
  searchPlaceholder,
  searchQuery = '',
  onSearchChange,
  allCategories = [],
  selectedCategories = [],
  onSelectCategory,
  children,
  categoryListSx,
}: CategoryFilterProps): ReactNode {
  const handleCategoryClick = useCallback(
    (category: string) => () => {
      onSelectCategory?.(category);
    },
    [onSelectCategory],
  );

  const searchLabel = searchPlaceholder ?? t('shared.ui.categoryFilter.search', 'Search');
  // Baseline `CategoryFilter.jsx`'s `styles.searchContainer(allCategories.length > 1)`:
  // the search box owns the whole gap down to the divider when there is no chip
  // row to sit between them.
  const showCategories = allCategories.length > 1;

  return (
    <Box sx={containerSx}>
      {title && (
        <Typography
          variant="headingSmall"
          color="text.secondary"
          sx={titleSx}
        >
          {title}
        </Typography>
      )}

      <Box
        data-category-filter-controls=""
        sx={controlsContainerSx}
      >
        <Box sx={showCategories ? searchContainerWithChipsSx : searchContainerSx}>
          <TextField
            placeholder={searchPlaceholder}
            value={searchQuery}
            onChange={onSearchChange}
            sx={searchFieldSx}
            variant="outlined"
            size="small"
            slotProps={{
              htmlInput: { 'aria-label': searchLabel },
              input: {
                sx: searchInputSx,
                startAdornment: (
                  <SearchIcon
                    sx={searchIconSx}
                    aria-hidden
                  />
                ),
              },
            }}
          />
        </Box>

        {showCategories && (
          <Box sx={combineSx(categoryFilterContainerSx, categoryListSx)}>
            <Box sx={categoryChipsWrapperSx}>
              {allCategories.map((category) => {
                const selected = selectedCategories.includes(category);
                return (
                  <Chip
                    key={category}
                    label={category}
                    clickable
                    aria-pressed={selected}
                    onClick={handleCategoryClick(category)}
                    sx={selected ? selectedCategoryChipSx : categoryChipSx}
                    slotProps={{ label: { sx: chipLabelSx } }}
                  />
                );
              })}
            </Box>
          </Box>
        )}
      </Box>

      <Box sx={itemsContainerSx}>{children}</Box>
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  width: '100%',
  height: '100%',
  boxSizing: 'border-box',
  overflow: 'hidden',
  backgroundColor: theme.vars.palette.background.chatBkg,
  paddingTop: theme.spacing(3),
});

const titleSx: SxProps<Theme> = (theme: Theme) => ({
  marginBottom: theme.spacing(2),
});

const controlsContainerSx: SxProps<Theme> = {
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
};

/** No chip row below: the search box carries the full 2rem gap to the divider. */
const searchContainerSx: SxProps<Theme> = (theme: Theme) => ({
  width: '23.75rem',
  maxWidth: '100%',
  marginBottom: theme.spacing(4),
  position: 'relative',
});

const searchContainerWithChipsSx: SxProps<Theme> = (theme: Theme) => ({
  width: '23.75rem',
  maxWidth: '100%',
  marginBottom: theme.spacing(2),
  position: 'relative',
});

const searchFieldSx: SxProps<Theme> = {
  width: '100%',
};

/** Baseline `searchIconContainer`: a 1rem glyph, not MUI's 1.25rem `fontSize="small"`. */
const searchIconSx: SxProps<Theme> = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.text.secondary,
});

/**
 * Styles the `OutlinedInput` slot directly (`TextField`'s `slotProps.input`
 * targets that component as a real prop, not a `.MuiOutlinedInput-*` class
 * selector) — R-T6 confines internal-DOM-selector overrides to
 * `shared/brand/mui-overrides/`, so this is the in-scope way to colour a
 * `TextField`'s input surface from a component file.
 */
const searchInputSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.bodyMedium,
  height: '2.25rem',
  color: theme.vars.palette.text.secondary,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  // Baseline `borderRadius: '1.75rem'` on a 2.25rem-tall field is a pill —
  // `radiusPill` is the token that says so (R-T10 bans the literal).
  borderRadius: theme.vars.shape.radiusPill,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  paddingLeft: theme.spacing(1.5),
  paddingRight: theme.spacing(1.5),
  gap: theme.spacing(1.5),
  transition: 'border 0.3s ease, background-color 0.3s ease',
  '&:hover': { borderColor: theme.vars.palette.border.hover },
  '&.Mui-focused': {
    borderColor: theme.vars.palette.border.flowNode,
    backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
  },
  '& input': {
    padding: 0,
    height: 'auto',
    '&::placeholder': { color: theme.vars.palette.text.disabled, opacity: 1 },
  },
  '& fieldset': { border: 'none' },
});

const categoryFilterContainerSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  maxWidth: '52.5rem',
  marginBottom: theme.spacing(2),
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
});

const categoryChipsWrapperSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexWrap: 'wrap',
  gap: theme.spacing(1),
  justifyContent: 'center',
  width: '100%',
});

/**
 * Baseline `categoryChip`/`selectedCategoryChip`: a 2rem pill with its own
 * padding and `labelSmall` type. The one deliberate substitution is the
 * radius — the baseline's `0.625rem` literal is banned by R-T10, and
 * `radiusMd` (0.5rem) is the nearest token.
 */
const baseCategoryChipSx = (theme: Theme) => ({
  ...theme.typography.labelSmall,
  height: '2rem',
  padding: theme.spacing(1, 2),
  borderRadius: theme.vars.shape.radiusMd,
  border: 'none',
  transition: 'all 0.2s ease-in-out',
  '&:hover': { backgroundColor: theme.vars.palette.background.button.secondary.hover },
});

const chipLabelSx: SxProps<Theme> = {
  padding: 0,
  textTransform: 'capitalize',
};

const categoryChipSx: SxProps<Theme> = (theme: Theme) => ({
  ...baseCategoryChipSx(theme),
  backgroundColor: theme.vars.palette.background.tag.default,
  color: theme.vars.palette.text.tag.default,
  boxShadow: theme.vars.palette.boxShadow.tag,
});

const selectedCategoryChipSx: SxProps<Theme> = (theme: Theme) => ({
  ...baseCategoryChipSx(theme),
  backgroundColor: theme.vars.palette.background.tag.selected,
  color: theme.vars.palette.text.tag.selected,
  boxShadow: 'none',
});

const itemsContainerSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  borderTop: `0.0625rem solid ${theme.vars.palette.border.table}`,
  background: theme.vars.palette.background.eliteaDefault,
  padding: theme.spacing(2, 3),
  overflowY: 'auto',
  overflowX: 'hidden',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  boxSizing: 'border-box',
  flex: 1,
  minHeight: 0,
  gap: theme.spacing(3),
});

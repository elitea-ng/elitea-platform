import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { CategoryItemCard, type CategoryItem } from '../CategoryItemCard';
import { combineSx } from '../lib/combineSx';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface CategorySectionProps {
  category: ReactNode;
  items: readonly CategoryItem[];
  /** Rendered in place of the item grid when `items` is empty. */
  emptyPlaceholder?: ReactNode;
  /** Hides the category title + divider, e.g. for a single-category list. @default true */
  showCategory?: boolean;
  sx?: SxProps<Theme>;
}

/**
 * A titled group of `CategoryItemCard` tiles. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/category/CategorySection.jsx`.
 *
 * Deviation from the baseline: `EmptyPlaceholder` (a prop named as if it
 * were a component, but always used as a plain node — `{EmptyPlaceholder ??
 * null}`, never `<EmptyPlaceholder />`) is renamed `emptyPlaceholder` to
 * match this codebase's prop-naming convention and to stop implying a
 * component-type prop it never was.
 */
export function CategorySection({
  category,
  items,
  emptyPlaceholder,
  showCategory = true,
  sx,
}: CategorySectionProps): ReactNode {
  return (
    <Box
      sx={combineSx(
        (theme: Theme) => ({
          width: '100%',
          maxWidth: '52.5rem',
          marginTop: showCategory ? 0 : theme.spacing(2),
          marginBottom: theme.spacing(4),
        }),
        sx,
      )}
    >
      {showCategory && (
        <Typography
          variant="subtitle"
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.groupedTitle.default })}
        >
          {category}
        </Typography>
      )}
      {showCategory && (
        <Box
          sx={(theme: Theme) => ({
            width: '100%',
            height: '0.0625rem',
            backgroundColor: theme.vars.palette.border.table,
            marginTop: theme.spacing(0.625),
            marginBottom: theme.spacing(1.25),
          })}
        />
      )}
      <Box sx={(theme: Theme) => ({ display: 'flex', flexWrap: 'wrap', gap: theme.spacing(1) })}>
        {items.map((item) => (
          <CategoryItemCard
            key={item.key}
            label={item.label}
            icon={item.icon}
            onClick={item.onClick}
            disabled={item.disabled}
            disabledReason={item.disabledReason}
          />
        ))}
        {items.length === 0 ? (emptyPlaceholder ?? null) : null}
      </Box>
    </Box>
  );
}

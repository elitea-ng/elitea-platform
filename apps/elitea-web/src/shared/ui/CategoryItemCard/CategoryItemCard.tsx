import { type ReactNode, useCallback } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';
import { useTextOverflow } from '../lib/useTextOverflow';

/** @public One selectable entry inside a `CategorySection` / `GroupedCategory` grid. */
export interface CategoryItem {
  key: string;
  label: ReactNode;
  icon?: ReactNode;
  onClick?: () => void;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface CategoryItemCardProps {
  label: ReactNode;
  icon?: ReactNode | undefined;
  onClick?: (() => void) | undefined;
  sx?: SxProps<Theme>;
  'data-testid'?: string;
}

/**
 * A fixed-size, icon + label tile used for category item grids (tools,
 * agents, …). Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/category/CategoryItemCard.jsx`.
 *
 * Deviations from the baseline:
 *  - The baseline is a `Card` with only an `onClick` handler — a `div`
 *    with no keyboard affordance at all (same R-C1 class of defect
 *    `BannerMessage`'s port fixed). This is a real `ButtonBase` (native
 *    `<button>`), so `Enter`/`Space` activation, focus and `tabIndex` come
 *    from the browser instead of being hand-rolled.
 *  - The baseline's hover background branches in JavaScript
 *    (`palette.mode === 'dark' ? palette.background.tabPanel :
 *    palette.background.default`) — banned outright by `elitea/no-mode-branch`
 *    (R-T2: colour schemes are CSS-variable-resolved, never JS-branched).
 *    Ported via `theme.applyStyles('dark', …)`, MUI's own mechanism for
 *    exactly this (verified against
 *    `node_modules/@mui/system/createTheme/applyStyles.d.ts`).
 *  - `borderRadius: '0.5rem'` becomes `theme.vars.shape.radiusMd` (also
 *    literally 8px / 0.5rem in the default pack —
 *    `shared/brand/tokens/default.pack.json`) to satisfy
 *    `elitea/ad-hoc-radius` (R-T10).
 *  - The baseline's `itemKey` prop was destructured only to set `key=` on
 *    this component's own single root return element — `key` has no effect
 *    there (it only affects React's reconciliation of a list of *siblings*,
 *    and a component's own returned root is not one). Dead code; dropped.
 *    The real list key belongs to the caller's `.map` (`CategorySection`
 *    sets `key={item.key}` on each `<CategoryItemCard>` itself).
 *  - The overflow-triggered tooltip (baseline:
 *    `Tooltip.TypographyWithConditionalTooltip`, not part of this port's
 *    scope) is reimplemented directly on `useTextOverflow`
 *    (`shared/ui/lib/useTextOverflow.ts`) + MUI's own `Tooltip` — the first
 *    real consumer of that hook.
 */
export function CategoryItemCard({
  label,
  icon,
  onClick,
  sx,
  'data-testid': dataTestId,
}: CategoryItemCardProps): ReactNode {
  const { textRef, isOverflowing } = useTextOverflow(label);
  // `textRef` is `RefObject<HTMLElement | null>` (shared across every
  // `useTextOverflow` consumer, so it cannot be narrower). Typography's ref
  // here targets `HTMLSpanElement` specifically; a plain assignment inside a
  // callback ref is a safe WIDENING write (`HTMLSpanElement` into an
  // `HTMLElement | null` field), not a cast.
  const attachLabelRef = useCallback(
    (node: HTMLSpanElement | null) => {
      textRef.current = node;
    },
    [textRef],
  );

  return (
    <ButtonBase
      data-testid={dataTestId}
      onClick={onClick}
      sx={combineSx(
        (theme: Theme) => ({
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'flex-start',
          textAlign: 'left',
          gap: theme.spacing(1.5),
          width: '12.75rem',
          height: '2.5rem',
          minHeight: '2.5rem',
          maxHeight: '2.5rem',
          padding: `${theme.spacing(1)} ${theme.spacing(2.5)}`,
          boxSizing: 'border-box',
          flexShrink: 0,
          flexGrow: 0,
          borderRadius: theme.vars.shape.radiusMd,
          border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
          backgroundColor: theme.vars.palette.background.secondary,
          transition: 'all 0.2s ease-in-out',
          '&:hover': {
            backgroundColor: theme.vars.palette.background.default,
            boxShadow: theme.vars.palette.boxShadow.default,
            border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          },
        }),
        (theme: Theme) =>
          theme.applyStyles('dark', {
            '&:hover': { backgroundColor: theme.vars.palette.background.tabPanel },
          }),
        sx,
      )}
    >
      {icon && (
        <Box
          aria-hidden="true"
          sx={{
            width: '1.25rem',
            height: '1.25rem',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            flexShrink: 0,
            // Baseline `itemIconContainer`: the glyph fills the 1.25rem box
            // regardless of the intrinsic size of the SVG handed in.
            '& svg': { width: '100%', height: '100%' },
          }}
        >
          {icon}
        </Box>
      )}
      <Tooltip
        title={isOverflowing ? label : ''}
        placement="top"
      >
        <Typography
          ref={attachLabelRef}
          variant="bodyMedium"
          sx={(theme: Theme) => ({
            flex: 1,
            minWidth: 0,
            color: theme.vars.palette.text.secondary,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          })}
        >
          {label}
        </Typography>
      </Tooltip>
    </ButtonBase>
  );
}

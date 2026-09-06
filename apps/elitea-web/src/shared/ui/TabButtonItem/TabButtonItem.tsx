import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import ToggleButton from '@mui/material/ToggleButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

/** @public One button in a {@link TabGroupButtonProps.items} row. */
export interface TabGroupButtonItem {
  value: string;
  label?: string;
  icon?: ReactNode;
  tooltip?: string;
  disabled?: boolean;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface TabButtonItemProps {
  item: TabGroupButtonItem;
  disableTooltip?: boolean;
  /**
   * Styles for the button itself. A group that needs every button to share a
   * size passes it here, so the caller never has to reach from the group into
   * the button's internal DOM (R-T6, `elitea/no-mui-internal-selector`).
   */
  sx?: SxProps<Theme>;
}

/**
 * One button of a `TabGroupButton` row: an icon and/or label, wrapped in a
 * `Tooltip` unless `disableTooltip`. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/tab-group-button/TabButtonItem.jsx`.
 *
 * Deviations:
 *  - No `borderRadius`/`customSx`/`theme` props — the baseline computed a
 *    per-position corner radius (square except the outer ends of the group)
 *    via a raw `'0.5rem 0 0 0.5rem'`-shaped string. R-T10
 *    (`elitea/ad-hoc-radius`) has no token form for that "some corners
 *    round, some corners `0`" shorthand (`0` itself is a literal number,
 *    which the rule rejects same as any other ad-hoc value — the "member
 *    expression passes" escape hatch only covers a single uniform value,
 *    and `MuiButton.ts`'s own doc comment flags the same "no escape hatch
 *    for a `50%`/pill idiom" gap for T1/S1 to resolve). Buttons render with
 *    `MuiToggleButton.ts`'s un-set corner radius instead of the baseline's
 *    joined-pill shape — a visual parity gap traceable to that
 *    already-flagged, out-of-scope token gap, not silently invented here.
 *  - Icon-only buttons (no `item.label`) get an `aria-label` from
 *    `item.tooltip`/`value` — the baseline relied on the `Tooltip`'s hover
 *    text alone, which gives no accessible name to a screen reader or
 *    keyboard user (a `Tooltip` only adds `aria-describedby` while open).
 */
export function TabButtonItem({ item, disableTooltip, sx }: TabButtonItemProps): ReactNode {
  const tooltipTitle = item.tooltip ?? item.label ?? item.value;

  const button = (
    <ToggleButton
      value={item.value}
      disabled={item.disabled}
      aria-label={item.label ? undefined : tooltipTitle}
      sx={sx}
    >
      {item.icon}
      {item.label && (
        <Typography
          variant="labelSmall"
          sx={item.icon ? labelWithIconSx : undefined}
        >
          {item.label}
        </Typography>
      )}
    </ToggleButton>
  );

  if (disableTooltip) {
    return button;
  }

  return (
    <Tooltip
      title={tooltipTitle}
      placement="top"
    >
      <Box
        component="span"
        sx={tooltipWrapperSx}
      >
        {button}
      </Box>
    </Tooltip>
  );
}

const labelWithIconSx: SxProps<Theme> = (theme: Theme) => ({
  marginLeft: theme.spacing(0.5),
});

const tooltipWrapperSx: SxProps<Theme> = {
  display: 'inline-flex',
};

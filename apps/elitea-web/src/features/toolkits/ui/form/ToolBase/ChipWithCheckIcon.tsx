import type { ReactElement, ReactNode } from 'react';

import Chip from '@mui/material/Chip';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '@/shared/ui/lib/combineSx';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

/**
 * Ported from `apps/elitea-ui/src/components/ChipWithCheckIcon.jsx` (48
 * lines). Not promoted anywhere in this worktree and not owned by any other
 * Wave-2 sub-unit's mission brief (a plain `@/components/*` legacy widget,
 * not an `[fsd]/features/*` file) — ported locally, co-located with its one
 * real consumer, `ToolActionsItems.tsx`.
 *
 * DISCLOSED SIMPLIFICATION: the baseline's `& .MuiChip-icon { color: ... }`
 * internal-selector override is dropped (R-T6, `elitea/no-mui-internal-selector`
 * — deep `.Mui*` selectors are banned outside `shared/brand/mui-overrides/`,
 * and no `MuiChip` override key for the icon slot exists there). The icon
 * keeps MUI's own default `Chip` icon color instead of matching the label's
 * disabled/enabled text color exactly — a minor, disclosed visual
 * simplification, not a functional one.
 */
export interface ChipWithCheckIconProps {
  readonly isSelected: boolean;
  readonly label: ReactNode;
  readonly icon?: ReactElement;
  readonly clickable?: boolean;
  readonly onClick?: () => void;
  readonly sx?: SxProps<Theme> | undefined;
  readonly warning?: boolean;
}

export function ChipWithCheckIcon({
  isSelected,
  label,
  icon,
  clickable = true,
  onClick,
  sx,
  warning = false,
}: ChipWithCheckIconProps): ReactNode {
  return (
    <Chip
      clickable={clickable}
      sx={combineSx(chipLabelSx(warning, isSelected, !clickable), sx)}
      icon={icon ?? (isSelected ? <CheckedIcon /> : undefined)}
      label={label}
      onClick={clickable ? onClick : undefined}
    />
  );
}

function chipLabelSx(warning: boolean, isSelected: boolean, disabled: boolean) {
  return (theme: Theme) => ({
    gap: '0.5rem',
    // oxlint-disable-next-line elitea/ad-hoc-radius -- baseline literal (10px): `apps/elitea-ui/src/components/ChipWithCheckIcon.jsx:29` pins `borderRadius: '10px'` on this one component, and no radius token is 10 (they are 4/8/16). This read `theme.vars.shape.radiusMd ?? '0.625rem'`, where the fallback was dead because `radiusMd` is defined, so the chip rendered 8px.
    borderRadius: '0.625rem',
    px: '1rem',
    py: '0.5rem',
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
    color: !disabled ? theme.vars.palette.text.secondary : theme.vars.palette.text.disabled,
    background: warning
      ? theme.vars.palette.background.warningBkg
      : isSelected
        ? theme.vars.palette.split.pressed
        : theme.vars.palette.background.userInputBackground,
    border: warning ? `1px solid ${theme.vars.palette.warning.main}` : undefined,
  });
}

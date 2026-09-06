/**
 * SettingsToggleCard — the toggle-card convention Settings › Memory uses for
 * every one of its three switches.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/memory/
 * MemoryContextManagement.jsx:169-181` (`toggleSection` + `toggleContent`) —
 * a rounded card on `background.userInputBackground` with a `headingSmall`
 * title and a `bodySmall` description on the left and the switch on the right.
 *
 * The baseline repeats that block three times; this is it once.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BaseSwitch } from '@/shared/ui/BaseSwitch';

export interface SettingsToggleCardProps {
  title: ReactNode;
  description: ReactNode;
  checked: boolean;
  onToggle: (checked: boolean) => void;
  disabled?: boolean;
  /** Accessible name for the switch itself (the card title is not a `<label>`). */
  switchAriaLabel: string;
  'data-testid'?: string;
}

export function SettingsToggleCard({
  title,
  description,
  checked,
  onToggle,
  disabled = false,
  switchAriaLabel,
  'data-testid': dataTestId,
}: SettingsToggleCardProps) {
  return (
    <Box sx={cardSx}>
      <Box sx={textSx}>
        <Typography variant="headingSmall" color="text.secondary">
          {title}
        </Typography>
        <Typography variant="bodySmall">{description}</Typography>
      </Box>
      <BaseSwitch
        data-testid={dataTestId}
        checked={checked}
        onChange={(event: React.ChangeEvent<HTMLInputElement>) => onToggle(event.target.checked)}
        color="primary"
        disabled={disabled}
        aria-label={switchAriaLabel}
      />
    </Box>
  );
}

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '1rem',
  padding: '0.75rem 1rem',
  // oxlint-disable-next-line elitea/ad-hoc-radius -- 0.75rem is the baseline's own literal and the value the production card measures at; no `radius*` token holds 12px (they are 4/8/16/pill), so `radiusMd` rounded it to 8px.
  borderRadius: '0.75rem',
  backgroundColor: theme.vars.palette.background.userInputBackground,
});

const textSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
};

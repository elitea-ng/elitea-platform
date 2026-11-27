import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';
import type { SystemStyleObject } from '@mui/system';

import { visuallyHidden } from '@/shared/lib/visuallyHidden';

import { StatusDot } from './StatusDot';
import { toneColor, type StatusTone } from './tone';

/**
 * - `label` (default): the icon, or a dot, in the tone's colour and the word in body text, no border — the
 *   legacy run history (`RunStatusCell`), the status filter (`StatusOptionLabel`) and the token expiry
 *   (`ExpiryInDays`);
 * - `chip`: an outlined pill, the icon in the tone's colour, the word in body text — the legacy schedule run
 *   (`ScheduleRunStatusChip`, its default);
 * - `text`: the icon and the word in the tone's colour (a neutral word in body text), no border — `ScheduleRunStatusChip variant="text"`;
 * - `icon`: the icon alone; the word stays for screen readers — `ScheduleRunStatusChip variant="icon"`.
 */
export type StatusChipVariant = 'label' | 'chip' | 'text' | 'icon';

export interface StatusChipProps {
  readonly tone: StatusTone;
  /** The word for the state. With `variant="icon"` only screen readers get it. */
  readonly label: ReactNode;
  /** A `16px` icon drawn in `currentColor`; without one the status shows a dot. */
  readonly icon?: ReactNode;
  readonly variant?: StatusChipVariant;
  /** `label` only: the word in `role.textMuted`, for a state that is over (the legacy «Expired»). */
  readonly muted?: boolean;
  readonly 'data-testid'?: string | undefined;
}

/**
 * The status of ELITEA NG (ADR-0062 decision 4, the status family): one component for a state shown by colour.
 * The legacy app had no shared status chip either; this one is built from what the legacy screens do (the legacy
 * solution first). `chip`, `text` and `icon` are the three variants of
 * `apps/elitea-ui/src/[fsd]/features/schedule/ui/ScheduleRunStatusChip.jsx`; `label` is the dot or icon with a word
 * that three other legacy screens write by hand. The colour comes from the tone (`role.success`, `role.warning`,
 * `role.danger`, `role.signal`, `role.textDisabled`), never from the call site.
 *
 * The legacy chip was a MUI `Chip` restyled through its inner classes; here the pill is a `Box` with the same
 * measures (`1.625rem` high, the `role.border` outline, the `radiusLg` pill of the theme's chip, `labelSmall`), so no
 * inner MUI class is styled (R-T6).
 *
 * A coloured MUI `Chip` outside `shared/ui` is counted by theme-gate check 10 (`[status-chip]`); a tag or a value in
 * a field (a `Chip` with no `color`) is not a status and is not counted.
 *
 * Not ported: the schedule chip's spinner for a running state — no NG screen shows one on a status yet.
 */
export function StatusChip({
  tone,
  label,
  icon,
  variant = 'label',
  muted = false,
  'data-testid': dataTestId,
}: StatusChipProps): ReactNode {
  // The word of `text` takes the tone's colour; a neutral word stays body text (legacy cancelled and skipped runs
  // are `text.secondary`, = `role.text`), as the disabled colour is too faint for text (axe, 3.5:1 in dark).
  const toneSx = (theme: Theme) => ({
    '--status-tone': toneColor(theme, tone),
    '--status-word': tone === 'neutral' ? theme.vars.palette.role.text : toneColor(theme, tone),
  });
  const mark = (
    <Box
      component="span"
      aria-hidden
      sx={markSx}
    >
      {icon ?? <StatusDot tone={tone} />}
    </Box>
  );

  if (variant === 'icon') {
    return (
      <Box
        component="span"
        data-testid={dataTestId}
        data-tone={tone}
        data-variant={variant}
        sx={[iconOnlySx, toneSx]}
      >
        {mark}
        <Box
          component="span"
          sx={visuallyHidden}
        >
          {label}
        </Box>
      </Box>
    );
  }

  return (
    <Box
      component="span"
      data-testid={dataTestId}
      data-tone={tone}
      data-variant={variant}
      sx={[rowSx, variant === 'chip' && chipSx, variant === 'text' && textSx, toneSx]}
    >
      {mark}
      <Typography
        component="span"
        variant={variant === 'label' ? 'bodySmall' : 'labelSmall'}
        color={variant === 'text' ? 'inherit' : muted && variant === 'label' ? 'role.textMuted' : 'role.text'}
        sx={wordSx}
      >
        {label}
      </Typography>
    </Box>
  );
}

/** The icon slot: `16px`, in the tone's colour (the svg icons fill with `currentColor`). */
const markSx: SystemStyleObject<Theme> = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  flexShrink: 0,
  color: 'var(--status-tone)',
  '& svg': { width: '1rem', height: '1rem' },
};

// Legacy `ExpiryInDays`: the icon and the word `8px` apart (scale step 1).
const rowSx: SystemStyleObject<Theme> = { display: 'inline-flex', alignItems: 'center', gap: 1, minWidth: 0, maxWidth: '100%' };

// Legacy schedule chip: `1.625rem` high, the outline `border.lines` (= `role.border`) on the outlined chip's fill
// `background.eliteaDefault`, the theme's `radiusLg` pill; MUI's small-chip icon inset `4px`, the label padded `8px`.
const chipSx: (theme: Theme) => SystemStyleObject<Theme> = (theme: Theme) => ({
  boxSizing: 'border-box',
  height: '1.625rem',
  gap: 0.5,
  paddingLeft: 0.5,
  paddingRight: 1,
  border: `0.0625rem solid ${theme.vars.palette.role.border}`,
  borderRadius: theme.vars.shape.radiusLg,
  background: theme.vars.palette.background.eliteaDefault,
});

// Legacy `text`: no outline and no fill, the word in the tone's colour (a neutral one in body text), `4px` from the
// icon.
const textSx: SystemStyleObject<Theme> = { gap: 0.5, color: 'var(--status-word)' };

const wordSx: SystemStyleObject<Theme> = { minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };

// Legacy: a `20px` box around the icon.
const iconOnlySx: SystemStyleObject<Theme> = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: '1.25rem',
  height: '1.25rem',
};

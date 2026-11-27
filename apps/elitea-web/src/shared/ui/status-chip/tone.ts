import type { Theme } from '@mui/material/styles';

/**
 * The states a status can show. A screen maps its own states onto these
 * (a token that expires soon is `warning`, a failed run is `danger`).
 */
export type StatusTone = 'success' | 'warning' | 'danger' | 'info' | 'neutral';

/**
 * The colour of a tone, as a role (`shared/brand/tokens/legacy-roles.json`):
 * the legacy screens used `status.published` / `success.main` for success,
 * `status.onModeration` / `warning.main` for warning, `text.error` for danger,
 * `info.main` for info and `icon.fill.disabled` for a state with no colour —
 * the same values as these roles in both schemes.
 */
export function toneColor(theme: Theme, tone: StatusTone): string {
  // Static dotted paths only: the reference scan of theme-gate check 7 reads no aliased palette.
  switch (tone) {
    case 'success':
      return theme.vars.palette.role.success;
    case 'warning':
      return theme.vars.palette.role.warning;
    case 'danger':
      return theme.vars.palette.role.danger;
    case 'info':
      return theme.vars.palette.role.signal;
    case 'neutral':
      return theme.vars.palette.role.textDisabled;
  }
}

import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/**
 * `MuiAvatar` (R-T12). Colours ported verbatim from `MainTheme.js:209-216`.
 *
 * MUI's default avatar is 40px with 20px initials — off the ladder at the
 * default pack's body size. The default (40px) avatar's initials are
 * `headingMedium`; an avatar sized anywhere else sets its initials through
 * `avatarInitialsType(sizePx)` (`shared/brand/typography.ts`) so they snap
 * to the ladder instead of scaling continuously with the circle.
 */
export const MuiAvatar: EliteaComponents['MuiAvatar'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      ...typeScale(theme.typography.headingMedium),
      background: theme.vars.palette.background.avatar,
      color: theme.vars.palette.text.default,
    }),
  },
};

import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/**
 * `MuiMenuItem` (R-T12). Simplified from
 * `apps/elitea-ui/src/theme/menuListVariants.js`'s `eliteaMenuItemVariants`
 * to the default (untagged) variant, for the same reason as `MuiList`
 * (see that file's comment): the baseline's other variants read
 * un-tokenised MUI `palette.action`/`error` roles with no shared/ui call
 * site. `background.select.hover` / `background.select.selected.default`
 * are this app's real hover/selected roles (used identically by
 * `SingleSelectMenuItem` and `MuiTreeItem`).
 */
export const MuiMenuItem: EliteaComponents['MuiMenuItem'] = {
  variants: [
    {
      props: {},
      style: ({ theme }) => ({
        // A menu item reads like the field that opened it: bodyMedium, not
        // MUI's stock 16px body1 (typography spec §2).
        ...typeScale(theme.typography.bodyMedium),
        paddingTop: theme.spacing(1),
        paddingBottom: theme.spacing(1),
        paddingLeft: theme.spacing(2),
        paddingRight: theme.spacing(2),
        minHeight: theme.spacing(5),
        '&:hover': {
          backgroundColor: theme.vars.palette.background.select.hover,
        },
        '&.Mui-selected': {
          backgroundColor: theme.vars.palette.background.select.selected.default,
          '&:hover': {
            backgroundColor: theme.vars.palette.background.select.selected.hover,
          },
        },
        '&.Mui-disabled': {
          opacity: 0.5,
        },
      }),
    },
  ],
};

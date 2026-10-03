import type { EliteaComponents } from '../theme-types';

/**
 * `MuiListItemText` (R-T12; typography spec §2). A list's primary line is
 * `bodyMedium` and its secondary line (metadata, timestamps) `bodySmall`.
 * MUI renders these as `body1`/`body2`, which would put the secondary line
 * at the same 14px as the primary one.
 *
 * `defaultProps.slotProps` is merged per slot with a call site's own
 * `slotProps` (MUI's `resolveProps`), so a site that passes, say,
 * `slotProps.primary.sx` keeps this variant. The secondary slot keeps its
 * old `<p>` element; the primary one stays a `<span>`, as MUI renders it.
 */
export const MuiListItemText: EliteaComponents['MuiListItemText'] = {
  defaultProps: {
    slotProps: {
      primary: { variant: 'bodyMedium' },
      secondary: { variant: 'bodySmall', component: 'p' },
    },
  },
};

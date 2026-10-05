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
 *
 * Colour: MUI paints the secondary line `textSecondary`, which in this app's
 * INVERTED tokens is the STRONG one, while the primary line inherits body
 * text (the dim `text.primary`). With the secondary line now a size smaller,
 * that read as small, loud captions under muted titles. The secondary line
 * is set to `textPrimary` (dim) instead, so size alone ranks the two lines.
 * The primary line keeps inheriting: nav items colour their selected and
 * hover states on the `ListItemButton`, and a fixed colour here would
 * override them. `color` is a Typography prop on a variant with no colour
 * of its own, so it is not the heading-variant no-op trap.
 */
export const MuiListItemText: EliteaComponents['MuiListItemText'] = {
  defaultProps: {
    slotProps: {
      primary: { variant: 'bodyMedium' },
      secondary: { variant: 'bodySmall', component: 'p', color: 'textPrimary' },
    },
  },
};

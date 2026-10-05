import type { EliteaComponents } from '../theme-types';
import { labelShrinkScale, typeScale } from '../typeScale';

/**
 * `MuiInputLabel` (R-T12; typography spec §2/§3). A field label is
 * `labelMedium`; a SHRUNK (floating) label sits exactly on rung −1.
 *
 * MUI shrinks with a hard-coded `scale(0.75)`, designed for 16px labels. At
 * this app's 14px label that rendered a 10.5px label: under the 12px floor
 * and off the ladder. The factor here is `k = rung(−1) / rung(0)`, computed
 * from the BUILT ladder (`labelShrinkScale`), never a constant, so the
 * shrunk label is a ladder rung for every brand pack.
 *
 * The translate offsets are MUI's own per-variant values (InputLabel.js),
 * restated because a CSS `transform` cannot be patched one function at a
 * time. `maxWidth` is MUI's `133%` (= 1 / 0.75) recomputed for `k`.
 * `MuiOutlinedInput.tsx` sizes the notch legend to match.
 */
const SHRUNK_TRANSLATE = {
  standard: { medium: 'translate(0, -1.5px)', small: 'translate(0, -1.5px)' },
  filled: { medium: 'translate(12px, 7px)', small: 'translate(12px, 4px)' },
  outlined: { medium: 'translate(14px, -9px)', small: 'translate(14px, -9px)' },
} as const;

/** MUI's per-variant horizontal allowance subtracted from the shrunk width. */
const SHRUNK_GUTTER = { standard: '0px', filled: '24px', outlined: '32px' } as const;

export const MuiInputLabel: EliteaComponents['MuiInputLabel'] = {
  styleOverrides: {
    root: ({ theme }) => typeScale(theme.typography.labelMedium),
    shrink: ({ theme, ownerState }) => {
      const k = labelShrinkScale(theme.typography);
      const variant = ownerState.variant ?? 'outlined';
      const size = ownerState.size === 'small' ? 'small' : 'medium';
      const widthPct = Number((100 / k).toFixed(2));
      return {
        transform: `${SHRUNK_TRANSLATE[variant][size]} scale(${k})`,
        maxWidth: `calc(${widthPct}% - ${SHRUNK_GUTTER[variant]})`,
      };
    },
  },
};

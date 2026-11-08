// @ts-nocheck
/**
 * ParticipantsWrapper styles and layout utilities.
 *
 * Extracted from `ParticipantsWrapper.tsx` to keep that file under 400 lines.
 */

import type { Theme } from '@mui/material/styles';

/**
 * Computes responsive grid sizes for the wrapper.
 */
function useWrapperGridSizes(collapsed: boolean): { xsSize: number; lgSize: number } {
  return { xsSize: 12, lgSize: collapsed ? 0.5 : 3 };
}

/**
 * Derives responsive styling properties from layout state.
 *
 * `collapsed` fixes adversarial review C5-wrapper #7: old-app
 * `ParticipantsWrapper.jsx`'s `largeScreenParticipantsWrapper` forces the
 * panel to a fixed `5rem` width when collapsed (`maxWidth: collapsed ?
 * '5rem !important' : ...`), regardless of the configured `panelWidth`. This
 * only applies on large screens — the small-window branch never shows the
 * collapsed icon-strip (see `Participants.tsx`'s `showCollapsedParticipants`),
 * so `collapsed` is intentionally not read there.
 */
export function deriveWrapperStyleParams(isSmallWindow: boolean, panelWidth: number, collapsed: boolean): {
  height: string;
  marginBottom: string | number;
  maxWidth: string;
  minWidth: string;
} {
  if (isSmallWindow) {
    return {
      height: 'auto',
      marginBottom: '1rem',
      maxWidth: `${panelWidth}px`,
      minWidth: `${panelWidth}px`,
    };
  }
  const width = collapsed ? '5rem' : `${panelWidth}px`;
  return {
    height: '100%',
    marginBottom: 0,
    maxWidth: width,
    minWidth: width,
  };
}

/**
 * Layout padding for the wrapper based on collapsed state.
 */
export function derivePaddingLeft(collapsed: boolean): { lg: string } {
  return { lg: collapsed ? '1.25rem' : '1.5rem' };
}

/**
 * Responsive sizing styles for the wrapper Grid cell.
 * Matches the old-app breakpoint logic: full-width on small screens,
 * fixed-width on large screens with collapsed sub-mode.
 */
export const wrapperSx = (
  _theme: Theme,
  params: ReturnType<typeof deriveWrapperStyleParams>,
  paddingLeft: ReturnType<typeof derivePaddingLeft>,
): React.CSSProperties => ({
  ...params,
  boxSizing: 'border-box',
  paddingRight: '1rem',
  paddingLeft,
});

/** The wrapper's grid sizes, style parameters and padding, together. */
export function useResponsiveSizes(collapsed: boolean, isSmallWindow: boolean, panelWidth: number) {
  const { xsSize, lgSize } = useWrapperGridSizes(collapsed);
  const styleParams = deriveWrapperStyleParams(isSmallWindow, panelWidth, collapsed);
  const paddingLeft = derivePaddingLeft(collapsed);
  return { xsSize, lgSize, styleParams, paddingLeft };
}

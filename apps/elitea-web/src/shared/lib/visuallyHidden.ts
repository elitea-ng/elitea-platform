/**
 * The standard screen-reader-only style: in the accessibility tree, out of the
 * layout. axe also counts it as off screen, so text inside it is not part of a
 * control's visible label.
 */
export const visuallyHidden = {
  position: 'absolute',
  width: '0.0625rem',
  height: '0.0625rem',
  overflow: 'hidden',
  clipPath: 'inset(50%)',
  whiteSpace: 'nowrap',
} as const;

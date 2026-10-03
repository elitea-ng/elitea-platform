import type { MouseEvent, ReactNode } from 'react';

import Button from '@mui/material/Button';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';
import { t } from '@/shared/i18n';

export interface ShowMoreButtonProps {
  /** True when the list or the text is open. The label then reads "Show less". */
  readonly expanded: boolean;
  readonly onClick: (event: MouseEvent<HTMLButtonElement>) => void;
  /** @default "Show more" */
  readonly moreLabel?: string;
  /** @default "Show less" */
  readonly lessLabel?: string;
  /** The id of the region this button opens and closes. It sets `aria-controls`. */
  readonly controls?: string;
  readonly sx?: SxProps<Theme>;
  readonly 'data-testid'?: string;
}

/**
 * The one "Show more" / "Show less" control (#6640).
 *
 * Each list used to draw its own: a `Typography` with an `onClick` that
 * underlined on hover, a `<button>` that dimmed on hover, a `Box` that took
 * the click and a `Typography` inside it that took the colour. They differed
 * in font size, weight, colour and hover, and only one of them was a real
 * button. This is the design's Auxiliary button: the `auxiliary` variant of
 * MUI `Button` (`shared/brand/mui-overrides/MuiButton.ts`), so the colour,
 * the hover and the pressed colour come from the brand pack. It has no
 * underline, 12px text at weight 500, and no padding of its own: the caller
 * places it with `sx`.
 */
export function ShowMoreButton({
  expanded,
  onClick,
  moreLabel,
  lessLabel,
  controls,
  sx,
  'data-testid': dataTestId,
}: ShowMoreButtonProps): ReactNode {
  const label = expanded
    ? (lessLabel ?? t('shared.ui.showMoreButton.less', 'Show less'))
    : (moreLabel ?? t('shared.ui.showMoreButton.more', 'Show more'));
  return (
    <AuxiliaryTextButton
      aria-expanded={expanded}
      {...(controls === undefined ? {} : { 'aria-controls': controls })}
      {...(dataTestId === undefined ? {} : { 'data-testid': dataTestId })}
      onClick={onClick}
      {...(sx === undefined ? {} : { sx })}
    >
      {label}
    </AuxiliaryTextButton>
  );
}

export interface AuxiliaryTextButtonProps {
  readonly children: ReactNode;
  readonly onClick: (event: MouseEvent<HTMLButtonElement>) => void;
  readonly sx?: SxProps<Theme>;
  readonly 'aria-expanded'?: boolean;
  readonly 'aria-controls'?: string;
  readonly 'data-testid'?: string;
}

/**
 * The text-only Auxiliary button that `ShowMoreButton` is made of, for a
 * text action that does not open and close a region (for example "Show" a
 * dialog). Same colour, hover, size and weight as "Show more".
 */
export function AuxiliaryTextButton({ children, onClick, sx, ...rest }: AuxiliaryTextButtonProps): ReactNode {
  return (
    <Button
      variant="auxiliary"
      disableRipple
      {...rest}
      onClick={onClick}
      sx={combineSx(buttonSx, sx)}
    >
      {children}
    </Button>
  );
}

const buttonSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: 0,
  padding: 0,
  // `labelSmall` is the 12px, weight-500 rung of the type scale the design names.
  ...theme.typography.labelSmall,
  textTransform: 'none',
  textDecoration: 'none',
  '&:hover, &:focus-visible': { textDecoration: 'none', backgroundColor: 'transparent' },
});

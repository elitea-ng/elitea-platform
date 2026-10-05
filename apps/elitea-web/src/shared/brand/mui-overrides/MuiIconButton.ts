import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiIconButton` (R-T12). Ported from
 * `apps/elitea-ui/src/components/IconButton.jsx`'s `eliteaIconButtonStyle`,
 * scoped to the colours `shared/ui` actually needs — `primary`, `secondary`
 * (MUI's own colour union, no augmentation needed), plus `tertiary`/`alarm`
 * (added to `IconButtonPropsColorOverrides` in `theme.augment.d.ts`; see
 * that file for why `tertiaryCount`/`magicAssistant`/`delete` are out of
 * scope). `IconButton` has no typed `variant` prop, and this was the app's
 * only icon-button skin, so the base geometry lives in `styleOverrides.root`
 * (applies unconditionally) instead of a `variant="elitea"` gate — the
 * per-colour `variants` entries below key on `color`, which IS typed.
 */
export const MuiIconButton: EliteaComponents['MuiIconButton'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      display: 'flex',
      height: '1.75rem',
      width: '1.75rem',
      padding: theme.spacing(0.75),
      alignItems: 'center',
      justifyContent: 'center',
      gap: theme.spacing(0.5),
      borderRadius: theme.vars.shape.radiusLg,
      fontFamily: theme.typography.fontFamily,
      ...typeScale(theme.typography.bodySmall),
      textTransform: 'none',
    }),
  },
  variants: [
    {
      props: { color: 'primary' },
      style: ({ theme }) => ({
        color: theme.vars.palette.text.button.primary,
        background: theme.vars.palette.background.button.primary.default,
        '&:hover': { background: theme.vars.palette.background.button.primary.hover },
        '&:active': { background: theme.vars.palette.background.button.primary.pressed },
        '&:disabled': {
          color: theme.vars.palette.text.button.primary,
          background: theme.vars.palette.background.button.primary.disabled,
        },
      }),
    },
    {
      props: { color: 'secondary' },
      style: ({ theme }) => ({
        color: theme.vars.palette.text.secondary,
        background: theme.vars.palette.background.button.secondary.default,
        '& .MuiSvgIcon-root path': { fill: theme.vars.palette.text.secondary },
        '&:hover': { background: theme.vars.palette.background.button.secondary.hover },
        '&:active': {
          color: theme.vars.palette.text.primary,
          background: theme.vars.palette.background.button.secondary.pressed,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        },
        '&:disabled': {
          color: theme.vars.palette.text.button.disabled,
          background: theme.vars.palette.background.button.default,
          '& .MuiSvgIcon-root path': { fill: theme.vars.palette.icon.fill.disabled },
        },
      }),
    },
    {
      props: { color: 'tertiary' },
      style: ({ theme }) => ({
        color: theme.vars.palette.text.default,
        background: 'transparent',
        minWidth: '1.75rem',
        height: '1.75rem',
        // Baseline `IconButton.jsx:66,122` is `16px` on a 28px-tall box — a
        // full pill. `radiusMd` (8px) rendered these as rounded rectangles.
        borderRadius: theme.vars.shape.radiusPill,
        padding: theme.spacing(0.75),
        '& .MuiSvgIcon-root path': { fill: theme.vars.palette.icon.fill.default },
        '&:hover': {
          background: theme.vars.palette.background.button.secondary.default,
          color: theme.vars.palette.text.secondary,
          '& .MuiSvgIcon-root path': { fill: theme.vars.palette.icon.fill.secondary },
        },
        '&:active': {
          color: theme.vars.palette.text.primary,
          background: theme.vars.palette.background.button.secondary.pressed,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        },
        '&:disabled': {
          color: theme.vars.palette.text.button.disabled,
          background: 'transparent',
          '& .MuiSvgIcon-root path': { fill: theme.vars.palette.icon.fill.disabled },
        },
      }),
    },
    {
      props: { color: 'alarm' },
      style: ({ theme }) => ({
        color: theme.vars.palette.text.secondary,
        background: theme.vars.palette.background.button.alarm.default,
        minWidth: '1.75rem',
        height: '1.75rem',
        // Baseline `IconButton.jsx:66,122` is `16px` on a 28px-tall box — a
        // full pill. `radiusMd` (8px) rendered these as rounded rectangles.
        borderRadius: theme.vars.shape.radiusPill,
        padding: theme.spacing(0.75),
        gap: theme.spacing(1.25),
        '&:hover': { background: theme.vars.palette.background.button.alarm.hover },
        '&:active': {
          color: theme.vars.palette.text.primary,
          background: theme.vars.palette.background.button.alarm.pressed,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        },
        '&:disabled': {
          color: theme.vars.palette.text.button.primary,
          background: theme.vars.palette.background.button.alarm.disabled,
        },
      }),
    },
  ],
};

import type { Theme } from '@mui/material/styles';

/**
 * Style factory for `UserInput.tsx`, split out to keep that file under the
 * §3.5 file-length/complexity budgets. Ported from `UserInput.jsx`'s own
 * `userInputStyles`, with brand-token substitutions — see each function's
 * own comment for the specific R-T1/R-T2/R-T9/R-T10/R-T11 deviation.
 */

export const stopIconStyle = { width: '1rem', height: '1rem' };

/**
 * `getInputBackground` reproduces `UserInput.jsx`'s two-tier background exactly:
 *
 *   not focused / not recording -> palette.background.card.default
 *   focused or recording        -> palette.mode === 'light'
 *                                    ? palette.background.secondary
 *                                    : palette.background.onboardingBody
 *
 * This used to read `userInputBackground` / `userInputBackgroundActive`
 * instead, on the reasoning that those tokens are "purpose-built for exactly
 * this component". Their NAMES say so; the baseline does not. `UserInput.jsx`
 * never reads either one — they are a 5%/10% translucent overlay pair
 * (`dark5`/`dark10`, `white5`/`white10`) that the baseline spends on other
 * surfaces (MemoryContextManagement, UsageDailyChart, SettingsDrawer), and our
 * pack carries the same values. So the composer rendered as a 5-10% wash over
 * the page rather than a filled surface, and a focused composer in particular
 * read as unfilled — reported from a side-by-side screenshot.
 *
 * The mode branch is still not reproduced (R-T2 bans `palette.mode`), and does
 * not need to be: `background.card.hover` already carries the baseline's two
 * focused colours, one per scheme — light `#FFFFFF` (the baseline's
 * `background.secondary` there) and dark `rgba(12, 17, 25, 1)` (its
 * `background.onboardingBody`). It is chosen for those VALUES, which match the
 * baseline pair exactly in both schemes, not for its name.
 */
/**
 * The desktop's composer (`native`): a plain text box the way a native app
 * draws one — a hairline border, an accent ring on focus, no gradient rim
 * or glow — with 20px lines (`COMPOSER_LINE_PX`, the resize handle's step)
 * and a compact footer row.
 */
function nativeContainer(theme: Theme, isFocused: boolean, isDragOver: boolean, isRecording: boolean) {
  const active = isFocused || isRecording;
  return {
    display: 'flex',
    flexDirection: 'column',
    width: '100%',
    padding: '.5rem .5rem .375rem .75rem',
    alignItems: 'flex-start',
    borderRadius: theme.vars.shape.radiusMd,
    background: isDragOver ? theme.vars.palette.background.button.drawerMenu.hover : theme.vars.palette.background.card.hover,
    border: `1px ${isDragOver ? 'dashed' : 'solid'} ${active || isDragOver ? theme.vars.palette.primary.main : theme.vars.palette.divider}`,
    // The focus ring: the accent border doubled, as a native text view draws it.
    boxShadow: active ? `0 0 0 1px ${theme.vars.palette.primary.main}` : 'none',
    boxSizing: 'border-box',
    gap: '.375rem',
    transition: 'border-color 120ms, box-shadow 120ms',
    position: 'relative',
  };
}

export function userInputStyles(isFocused: boolean, isDragOver: boolean, isRecording: boolean, native = false) {
  const base = webUserInputStyles(isFocused, isDragOver, isRecording);
  if (!native) return base;
  return {
    ...base,
    gradientBorder: (theme: Theme) => ({ width: '100%', padding: '0', borderRadius: theme.vars.shape.radiusMd, background: 'transparent' }),
    container: (theme: Theme) => nativeContainer(theme, isFocused, isDragOver, isRecording),
    mirrorDiv: { ...base.mirrorDiv, lineHeight: '1.25rem' },
    textField: { ...base.textField, lineHeight: '1.25rem' },
    footer: { ...base.footer, minHeight: '1.75rem' },
  };
}

function webUserInputStyles(isFocused: boolean, isDragOver: boolean, isRecording: boolean) {
  const getInputBackground = (theme: Theme): string =>
    isFocused || isRecording
      ? theme.vars.palette.background.card.hover
      : theme.vars.palette.background.card.default;

  const getInputBorder = (theme: Theme): string => {
    if (isRecording) return theme.vars.palette.primary.main;
    // Control boundary at rest (brand guide v3: borderStrong, ≥3:1).
    if (!isFocused) return theme.vars.palette.border.hover;
    // Flat focus ring colour (brand guide v3: no gradients in UI chrome).
    return theme.vars.palette.background.userInputBorderLight;
  };

  return {
    gradientBorder: (theme: Theme) => ({
      width: '100%',
      padding: '0.0625rem',
      borderRadius: theme.vars.shape.radiusLg,
      background: getInputBorder(theme),
      ...((isFocused || isRecording) && {
        boxShadow: isRecording
          ? `0 0 0.75rem 0 ${theme.vars.palette.primary.main}40`
          : `0 -0.3125rem 1.25rem 0 ${theme.vars.palette.background.userInputBorderShadow}`,
      }),
    }),
    container: (theme: Theme) => ({
      display: 'flex',
      flexDirection: 'column',
      width: '100%',
      padding: '.75rem 1rem',
      alignItems: 'flex-start',
      borderRadius: theme.vars.shape.radiusLg,
      background: isDragOver ? `${theme.vars.palette.primary.main}15` : getInputBackground(theme),
      border: isDragOver
        ? `0.125rem dashed ${theme.vars.palette.primary.main}`
        : `0.0625rem solid ${isFocused || isRecording ? 'transparent' : theme.vars.palette.border.lines}`,
      boxSizing: 'border-box',
      gap: '1.5rem',
      transition: 'all 0.2s ease-in-out',
      position: 'relative',
      ...(isDragOver && { boxShadow: `0 0.25rem 0.75rem ${theme.vars.palette.primary.main}30` }),
    }),
    textFieldWrapper: {
      flex: 1,
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      width: '100%',
      position: 'relative',
    },
    mirrorDiv: {
      position: 'absolute',
      inset: 0,
      overflow: 'auto',
      pointerEvents: 'none',
      zIndex: 0,
      whiteSpace: 'pre-wrap',
      wordBreak: 'break-word',
      padding: 0,
      fontStyle: 'normal',
      fontWeight: 500,
      lineHeight: '1.5rem',
      fontFamily: 'inherit',
      '&::-webkit-scrollbar': { display: 'none' },
      scrollbarWidth: 'none',
    },
    textField: {
      padding: 0,
      flex: '1 0 0',
      fontStyle: 'normal',
      fontWeight: 500,
      lineHeight: '1.5rem',
      position: 'relative',
      zIndex: 1,
      '&::-webkit-scrollbar': { display: 'none' },
      scrollbarWidth: 'none',
      '& textarea': { marginBottom: 0, '&::-webkit-scrollbar': { display: 'none' } },
    },
    textFieldInput: (color: string | undefined) => (theme: Theme) => ({
      color: color ?? theme.vars.palette.text.secondary,
      padding: 0,
    }),
    transparentCaretText: (color: string | undefined) => (theme: Theme) => ({
      '& textarea': { color: 'transparent', caretColor: color ?? theme.vars.palette.text.secondary },
    }),
    expandButton: { marginLeft: 0 },
    expandIcon: (iconColor: string | undefined) => (theme: Theme) => ({
      width: '1rem',
      height: '1rem',
      color: iconColor ?? theme.vars.palette.icon.fill.default,
    }),
    footer: {
      display: 'flex',
      justifyContent: 'space-between',
      width: '100%',
      alignItems: 'center',
      minHeight: '2.5rem',
      gap: { xs: '.5rem', sm: '0.5rem' },
    },
    sendButtonContainer: {
      display: 'flex',
      height: 'auto',
      alignItems: 'center',
      justifyContent: 'center',
    },
    stopButton: (iconColor: string | undefined) => (theme: Theme) => ({
      cursor: 'pointer',
      display: 'flex',
      flexDirection: 'row',
      justifyContent: 'center',
      alignItems: 'center',
      color: iconColor ?? theme.vars.palette.icon.fill.attention,
      marginLeft: 0,
    }),
  };
}

export type UserInputStyles = ReturnType<typeof webUserInputStyles>;

import type { EliteaComponents } from '../theme-types';

/**
 * `MuiCssBaseline` (R-T12). Ported verbatim from `MainTheme.js:188-208`: the
 * app hides the native scrollbar chrome (every scroll surface that needs a
 * visible thumb uses `ScrollableContainer`'s SimpleBar instead) and disables
 * the text caret outside actual inputs.
 *
 * Focus (brand guide v3): ONE keyboard focus ring for every element, a 2px
 * ring in the scheme's signal colour (`focus.ring`: cyan on Ink, indigo on
 * Paper) with a 2px offset, so it stays visible on an indigo button in the
 * light scheme where signal equals primary. `:focus-visible` only: a mouse
 * click shows no ring.
 */
export const MuiCssBaseline: EliteaComponents['MuiCssBaseline'] = {
  styleOverrides: (theme) => ({
    '*': {
      scrollbarWidth: 'none',
    },
    ':focus-visible': {
      outline: `2px solid ${theme.vars.palette.focus.ring}`,
      outlineOffset: '2px',
    },
    body: {
      caretColor: 'transparent',
      height: '100%',
      '::-webkit-scrollbar': {
        display: 'none',
      },
      msOverflowStyle: 'none',
    },
    input: {
      caretColor: 'auto',
    },
    textarea: {
      caretColor: 'auto',
    },
  }),
};

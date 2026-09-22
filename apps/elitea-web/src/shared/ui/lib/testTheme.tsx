import type { ReactElement, ReactNode } from 'react';

import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { render, type RenderResult } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

/**
 * Shared render helper for `shared/ui` component tests (spec §6.2).
 *
 * Every component reads `theme.vars.palette.*` (R-T7), so a bare
 * `@testing-library/react` `render()` with no `ThemeProvider` would throw
 * the moment a component touches `theme.vars` — this wraps the real Elitea
 * theme once so every test file does not repeat the boilerplate.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const wrap = (ui: ReactNode): ReactElement => (
  <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
    <CssBaseline />
    {ui}
  </ThemeProvider>
);

/**
 * `result.rerender(nextUi)` re-wraps `nextUi` in the same `ThemeProvider`
 * rather than replacing the whole tree with an unwrapped element — RTL's
 * own `rerender` re-renders exactly the element it is given, so calling it
 * with a bare (unwrapped) `nextUi` would unmount the `ThemeProvider` too and
 * every `theme.vars.*` read in the component under test would throw on the
 * next render.
 */
export function renderWithTheme(ui: ReactElement): RenderResult {
  const result = render(wrap(ui));
  return {
    ...result,
    rerender: (nextUi: ReactNode) => {
      result.rerender(wrap(nextUi));
    },
  };
}

/**
 * Converts a `rem` length to the `px` string `getComputedStyle` now reports.
 *
 * **Why this exists (jsdom 29 → 30).** jsdom@29 echoed the SPECIFIED value back
 * out of `getComputedStyle`, so `getComputedStyle(el).width` returned the very
 * `'13.5rem'` the component (or the theme) declared, and a test could compare
 * the two strings directly. jsdom@30 resolves relative lengths first and reports
 * `'216px'`. Six assertions across five files broke on exactly that, all of them
 * comparing a computed length against a `rem` declaration.
 *
 * Converting here rather than hard-coding the pixel literal at each call site is
 * what keeps those assertions pointed at the SHARED constant they were written
 * to watch — `AdminNav.test.tsx` asserts `SIDE_BAR_WIDTH_REM` precisely so the
 * admin rail and the app rail cannot drift apart behind two green tests, and a
 * literal `'216px'` there would throw that away.
 *
 * 16 is jsdom's root font size: its UA stylesheet leaves `html` at `font-size:
 * medium`, and this app never overrides it.
 *
 * Throws on any other unit rather than passing the value through — a helper that
 * quietly returns a `px` or `em` input unchanged would make a mismatched-unit
 * assertion pass for the wrong reason.
 */
export function remToPx(rem: string | number | undefined): string {
  const value = typeof rem === 'number' ? rem : typeof rem === 'string' ? /^(-?[\d.]+)rem$/.exec(rem)?.[1] : undefined;
  if (value === undefined) throw new Error(`remToPx expects a rem length, received ${JSON.stringify(rem)}`);
  return `${Number(value) * 16}px`;
}

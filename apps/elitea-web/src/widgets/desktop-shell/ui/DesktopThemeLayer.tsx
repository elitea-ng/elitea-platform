/**
 * The desktop's look, as one layer any desktop screen can sit under — the
 * signed-in app (via `NativeLook`) and the connect / sign-in screens (under
 * the same brand theme): `withDesktopComponents` on the theme the screen
 * already has, and the global rules a native app gets from its toolkit
 * (default cursor, keyboard-only focus ring, thin overlay scrollbars, chrome
 * that is chrome — see CHROME below).
 */
import { useEffect, type ReactNode } from 'react';

import GlobalStyles from '@mui/material/GlobalStyles';
import { ThemeProvider, type Theme } from '@mui/material/styles';

import { SYSTEM_FONT_STACK, withDesktopComponents } from '../lib/desktopTheme';

/**
 * The chrome: what a native app does not select on a stray drag — buttons,
 * tabs, menu items, the toolbar, the sidebar. Everything else (chat, agent
 * pages, alerts, errors, code) stays selectable, so it can be copied.
 */
const CHROME = [
  'button',
  '[role="button"]',
  '[role="tab"]',
  '[role="menuitem"]',
  '[role="menuitemradio"]',
  '[role="radio"]',
  '[role="separator"]',
  'label',
  'nav',
  '[data-chrome]',
  '[data-testid="title-bar"]',
].join(', ');

/** ⌘. cancels the sheet in front, as Escape does. */
function useCancelShortcut(): void {
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key !== '.' || !(event.metaKey || event.ctrlKey)) return;
      const sheets = document.querySelectorAll('[role="dialog"]');
      const front = sheets.item(sheets.length - 1);
      if (front === null) return;
      event.preventDefault();
      front.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', code: 'Escape', bubbles: true }));
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}

export function DesktopThemeLayer({ children, transparentPage = false }: { children: ReactNode; transparentPage?: boolean }): React.JSX.Element {
  useCancelShortcut();
  return (
    <ThemeProvider theme={withDesktopComponents}>
      <GlobalStyles
        styles={(theme: Theme) => ({
          body: {
            fontFamily: SYSTEM_FONT_STACK,
            fontSize: '0.8125rem',
            WebkitFontSmoothing: 'antialiased',
            cursor: 'default',
            ...(transparentPage ? { background: 'transparent' } : {}),
          },
          ...(transparentPage ? { html: { background: 'transparent' } } : {}),
          [CHROME]: { userSelect: 'none', WebkitUserSelect: 'none' },
          'input, textarea': { userSelect: 'text', WebkitUserSelect: 'text', cursor: 'text' },
          // Only real links are hands.
          'a[href], a[href] *': { cursor: 'pointer' },
          '[data-tauri-drag-region]': { cursor: 'default' },
          // A pointer never draws a ring; the keyboard draws the system accent's.
          '*:focus': { outline: 'none' },
          '*:focus-visible': {
            outline: `3px solid color-mix(in srgb, ${theme.vars.palette.primary.main} 45%, transparent)`,
            outlineOffset: 0,
            borderRadius: 4,
          },
          'input:focus-visible, textarea:focus-visible': { outline: 'none' },
          '::selection': { background: `color-mix(in srgb, ${theme.vars.palette.primary.main} 35%, transparent)` },
          // Thin overlay scrollbars, the thumb only as strong as the content under it.
          '*': { scrollbarWidth: 'thin', scrollbarColor: `color-mix(in srgb, ${theme.vars.palette.text.primary} 30%, transparent) transparent` },
          '*::-webkit-scrollbar': { width: 8, height: 8 },
          '*::-webkit-scrollbar-track': { background: 'transparent' },
          '*::-webkit-scrollbar-thumb': {
            borderRadius: 4,
            border: '2px solid transparent',
            backgroundClip: 'content-box',
            backgroundColor: `color-mix(in srgb, ${theme.vars.palette.text.primary} 30%, transparent)`,
          },
        })}
      />
      {children}
    </ThemeProvider>
  );
}

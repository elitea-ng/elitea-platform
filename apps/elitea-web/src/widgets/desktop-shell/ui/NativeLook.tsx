/**
 * The desktop's native typography: the platform's UI font (SF Pro on macOS,
 * Segoe UI on Windows) instead of the web's brand face, everywhere the
 * brand face was used — the monospace face stays. Colours, logos and the
 * scheme stay the deployment's (the brand pack is untouched): this only
 * swaps the family on the theme the app already has, for everything rendered
 * under it, portalled menus and dialogs included.
 *
 * When the host gives the window a native material (`vibrancy`), the page
 * background is cleared so the sidebar's tint can show it.
 */
import type { ReactNode } from 'react';

import GlobalStyles from '@mui/material/GlobalStyles';
import { ThemeProvider, type Theme } from '@mui/material/styles';

import { useDesktopLayout } from '../model/desktopLayout.store';

const SYSTEM_FONT_STACK =
  '-apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI Variable Text", "Segoe UI", system-ui, Roboto, "Helvetica Neue", Arial, sans-serif';

function withNativeFont(outer: Theme): Theme {
  const brandFamily = outer.typography.fontFamily;
  const typography: Record<string, unknown> = { ...outer.typography, fontFamily: SYSTEM_FONT_STACK };
  for (const [key, value] of Object.entries(outer.typography)) {
    if (typeof value === 'object' && value !== null && (value as { fontFamily?: unknown }).fontFamily === brandFamily) {
      typography[key] = { ...(value as object), fontFamily: SYSTEM_FONT_STACK };
    }
  }
  return { ...outer, typography: typography as unknown as Theme['typography'] };
}

export function NativeLook({ children }: { children: ReactNode }): React.JSX.Element {
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  return (
    <ThemeProvider theme={withNativeFont}>
      <GlobalStyles
        styles={(theme: Theme) => ({
          body: {
            fontFamily: SYSTEM_FONT_STACK,
            WebkitFontSmoothing: 'antialiased',
            ...(vibrancy ? { background: 'transparent' } : {}),
          },
          ...(vibrancy ? { html: { background: 'transparent' } } : {}),
          // Native apps do not select their chrome on a stray drag.
          '[data-tauri-drag-region]': { cursor: 'default' },
          '::selection': { background: theme.vars.palette.background.button.drawerMenu.selected },
        })}
      />
      {children}
    </ThemeProvider>
  );
}

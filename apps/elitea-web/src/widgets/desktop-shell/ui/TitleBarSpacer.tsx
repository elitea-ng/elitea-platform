/**
 * The top row of a pane. When the host draws the web content under the title
 * bar (`titlebar_overlay`), this row is the window's drag region and, in the
 * leftmost pane, keeps clear of the traffic lights; with normal chrome it is
 * an ordinary header row (the brand logo in the sidebar).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { BrandLogoFull } from '@/shared/ui/brand-logo';

import { useDesktopLayout } from '../model/desktopLayout.store';

/** The macOS title bar's height (the drag region the traffic lights sit in). */
const TITLE_BAR_REM = 2.375;
const REM_PX = 16;

export interface TitleBarSpacerProps {
  /** This row starts at the window's left edge: reserve the traffic lights' inset. */
  leading?: boolean;
  /** Show the brand logo when the window has normal chrome. */
  logo?: boolean;
  trailing?: ReactNode;
  children?: ReactNode;
}

export function TitleBarSpacer({ leading = false, logo = true, trailing, children }: TitleBarSpacerProps): React.JSX.Element {
  const platform = useDesktopLayout((state) => state.platform);
  const overlay = platform.titlebar_overlay;
  const insetRem = overlay && leading ? platform.traffic_light_inset_px / REM_PX : 0;
  return (
    <Box
      data-tauri-drag-region={overlay ? true : undefined}
      data-testid="title-bar"
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        flexShrink: 0,
        minHeight: `${String(TITLE_BAR_REM)}rem`,
        paddingLeft: insetRem > 0 ? `${String(insetRem)}rem` : 1.5,
        paddingRight: 1,
        userSelect: 'none',
        WebkitUserSelect: 'none',
      }}
    >
      {!overlay && leading && logo && <BrandLogoFull style={{ height: '1.125rem', width: 'auto' }} />}
      <Box data-tauri-drag-region={overlay ? true : undefined} sx={{ flex: 1, minWidth: 0, display: 'flex', alignItems: 'center', gap: 1 }}>
        {children}
      </Box>
      {trailing}
    </Box>
  );
}

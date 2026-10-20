/**
 * The desktop's native look for the signed-in app: `DesktopThemeLayer`
 * (platform font, compact controls, native focus/scroll behaviour), and —
 * when the host gives the window a native material (`vibrancy`) — a cleared
 * page background so the sidebar's tint can show it. Colours, logos and the
 * scheme stay the deployment's (the brand pack is untouched).
 */
import type { ReactNode } from 'react';

import { useDesktopLayout } from '../model/desktopLayout.store';
import { DesktopThemeLayer } from './DesktopThemeLayer';

export function NativeLook({ children }: { children: ReactNode }): React.JSX.Element {
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  return <DesktopThemeLayer transparentPage={vibrancy}>{children}</DesktopThemeLayer>;
}

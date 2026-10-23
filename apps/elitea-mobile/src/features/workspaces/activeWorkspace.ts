import { useMemo } from 'react';

import type { BrandInput } from '@/design/brand';
import { brandInputFrom } from '@/design/resolve';
import { useSession } from '@/state/session';
import type { BrandImage } from '@/theme/brandPack';
import { useBrandTheme } from '@/theme/brandStore';

export interface ActiveWorkspace {
  origin: string;
  /** The brand's product name, or null (unbranded: show the host). */
  name: string | null;
  logoMark: BrandImage | null;
  brand: BrandInput | null;
}

/**
 * The active workspace as its identity surfaces show it (the Home pill, the inline title, the
 * switcher's current row, sign-in, the lock screen): origin, brand name, mark and accent. The
 * theme counts only when it is the active origin's (never a previous workspace's).
 */
export function useActiveWorkspace(): ActiveWorkspace | null {
  const origin = useSession((s) => s.serverOrigin);
  const theme = useBrandTheme();
  return useMemo(() => {
    if (origin === null) return null;
    const own = theme !== null && theme.origin === origin ? theme : null;
    return {
      origin,
      name: own && !own.productDefault ? own.productName.trim() || null : null,
      logoMark: own?.logoMark ?? null,
      brand: brandInputFrom(own),
    };
  }, [origin, theme]);
}

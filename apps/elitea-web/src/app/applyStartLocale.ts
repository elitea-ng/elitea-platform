/**
 * Applies the person's language before the app's modules load: `t()` is a
 * plain function that some modules call at their top level, so the language
 * has to be in place before they are imported. The web entry (`./main.tsx`)
 * and the desktop entry (`entries/desktop/main.tsx`) both call this; the
 * Language choice in the account menu only saves the choice and reloads.
 *
 * A right-to-left language also installs the RTL emotion cache, loaded only
 * then (`./providers/BrandThemeProvider.tsx`).
 */
import { applyLocale, isRtl, startLocale } from '@/shared/i18n';

import { installRtlStyleCache } from './providers/BrandThemeProvider';

export async function applyStartLocale(): Promise<void> {
  const locale = startLocale();
  await applyLocale(locale);
  if (isRtl(locale)) installRtlStyleCache((await import('./providers/rtlStyleCache')).createRtlStyleCache());
}

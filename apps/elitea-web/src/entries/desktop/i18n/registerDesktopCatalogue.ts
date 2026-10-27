/**
 * The desktop-only strings (`en.desktop.json`): the workspace screens, the
 * desktop shell, the connect screen. They are kept out of
 * `src/shared/i18n/en.json` because that catalogue ships in the DEFAULT web
 * app's initial chunk, which never renders them (bundle budget). Only the
 * desktop entry imports this module, and it registers the bundle before its
 * first render, so `t()` resolves desktop keys exactly as it does shared ones.
 * Their translations (`fr-CA.desktop.json`, …) load with the shared ones when
 * a language is applied (`shared/i18n/locales.ts`).
 *
 * Which catalogue a key belongs to is decided by where it is called;
 * `scripts/i18n-backfill.mjs --check` enforces it (`isDesktopOnlyPath`).
 */
import { i18n, registerCatalogueLoader, type Catalogue, type TranslatedLocale } from '@/shared/i18n';

import catalogue from './en.desktop.json';

const LOCALE = 'en';
const NAMESPACE = 'translation';

const TRANSLATIONS: Record<TranslatedLocale, () => Promise<{ default: Catalogue }>> = {
  'fr-CA': () => import('./fr-CA.desktop.json'),
  es: () => import('./es.desktop.json'),
};

let loaderRegistered = false;

export function registerDesktopCatalogue(): void {
  // deep merge, no overwrite: a shared key is never shadowed by the desktop file.
  i18n.addResourceBundle(LOCALE, NAMESPACE, catalogue, true, false);
  if (loaderRegistered) return;
  loaderRegistered = true;
  registerCatalogueLoader(async (locale) => [(await TRANSLATIONS[locale]()).default]);
}

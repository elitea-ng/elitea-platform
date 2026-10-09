/**
 * The desktop-only strings (`en.desktop.json`): the workspace screens, the
 * desktop shell, the connect screen. They are kept out of
 * `src/shared/i18n/en.json` because that catalogue ships in the DEFAULT web
 * app's initial chunk, which never renders them (bundle budget). Only the
 * desktop entry imports this module, and it registers the bundle before its
 * first render, so `t()` resolves desktop keys exactly as it does shared ones.
 *
 * Which catalogue a key belongs to is decided by where it is called;
 * `scripts/i18n-backfill.mjs --check` enforces it (`isDesktopOnlyPath`).
 */
import { i18n } from '@/shared/i18n';

import catalogue from './en.desktop.json';

const LOCALE = 'en';
const NAMESPACE = 'translation';

export function registerDesktopCatalogue(): void {
  // deep merge, no overwrite: a shared key is never shadowed by the desktop file.
  i18n.addResourceBundle(LOCALE, NAMESPACE, catalogue, true, false);
}

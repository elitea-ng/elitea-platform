/**
 * The languages the app ships, and switching between them.
 *
 * `en` is bundled (`./i18n.ts`). Every other language is a lazily loaded
 * catalogue per `en` catalogue (`fr-CA.json` beside `en.json`, …), so a
 * person who never leaves English downloads none of them. A key a translation
 * lacks resolves to its English value (`fallbackLng`), never to the bare key.
 *
 * The choice is a per-device preference (`el.lang`, `shared/lib/storage.ts`).
 * Sign-out's sweep clears it like every `el.` key, so the next person at a
 * shared machine starts in English.
 *
 * `t()` is a plain function, not a hook, so nothing re-renders when the
 * language changes. A caller applies a new choice by saving it and reloading
 * the page, and an entry applies the saved one before it imports the modules
 * that call `t()` at their top level.
 */
import { createStorage } from '@/shared/lib/storage';

import { DEFAULT_LOCALE, DEFAULT_NAMESPACE, i18n } from './i18n';

export const LOCALES = [
  { code: 'en', label: 'English' },
  { code: 'fr-CA', label: 'Français (Canada)' },
  { code: 'es', label: 'Español' },
] as const;

export type Locale = (typeof LOCALES)[number]['code'];
export type TranslatedLocale = Exclude<Locale, 'en'>;
export type Catalogue = Record<string, string>;

const LOCALE_STORAGE_KEY = 'lang';

const SHARED_CATALOGUES: Record<TranslatedLocale, () => Promise<{ default: Catalogue }>> = {
  'fr-CA': () => import('./fr-CA.json'),
  es: () => import('./es.json'),
};

/** Loads an entry's own catalogues for a language (the desktop's `fr-CA.desktop.json`, …). */
export type CatalogueLoader = (locale: TranslatedLocale) => Promise<readonly Catalogue[]>;

const entryLoaders: CatalogueLoader[] = [];

export function registerCatalogueLoader(loader: CatalogueLoader): void {
  entryLoaders.push(loader);
}

/**
 * Registers a small catalogue's every language at once — the route catalogues
 * a lazily loaded module registers itself — so the strings are there in
 * whichever language is current when the module loads.
 */
export function addCatalogues(catalogues: Record<Locale, Catalogue>): void {
  for (const { code } of LOCALES) i18n.addResourceBundle(code, DEFAULT_NAMESPACE, catalogues[code], true, false);
}

function isLocale(value: unknown): value is Locale {
  return LOCALES.some(({ code }) => code === value);
}

export function storedLocale(): Locale {
  try {
    const value = createStorage('local').get(LOCALE_STORAGE_KEY);
    return isLocale(value) ? value : DEFAULT_LOCALE;
  } catch {
    return DEFAULT_LOCALE;
  }
}

export function saveLocale(locale: Locale): void {
  try {
    createStorage('local').set(LOCALE_STORAGE_KEY, locale);
  } catch {
    // Storage refused (private mode, quota): the choice lasts until the reload.
  }
}

export function currentLocale(): Locale {
  return isLocale(i18n.language) ? i18n.language : DEFAULT_LOCALE;
}

/** Loads `locale`'s catalogues and makes it the language `t()` answers in. A catalogue that fails to load leaves English. */
export async function applyLocale(locale: Locale): Promise<void> {
  if (locale !== DEFAULT_LOCALE) {
    const loaded = await Promise.all([
      SHARED_CATALOGUES[locale]().then((module) => [module.default]),
      ...entryLoaders.map((loader) => loader(locale)),
    ]);
    for (const catalogue of loaded.flat()) i18n.addResourceBundle(locale, DEFAULT_NAMESPACE, catalogue, true, false);
  }
  await i18n.changeLanguage(locale);
  document.documentElement.lang = locale;
}

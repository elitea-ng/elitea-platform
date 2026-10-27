/**
 * The languages the app ships, which one a person sees, and switching.
 *
 * `en` is bundled (`./i18n.ts`). Every other language is a set of lazily
 * loaded files beside each `en` catalogue — `de.json` beside `en.json`,
 * `de.desktop.json` beside `en.desktop.json`, `de.chatCloud.json` beside a
 * route catalogue — found by one `import.meta.glob`, so a person who stays in
 * English downloads none of them and a new catalogue needs no registration.
 * A key a translation lacks resolves to its English value (`fallbackLng`),
 * never to the bare key.
 *
 * Which language: the person's choice (`el.lang`, `shared/lib/storage.ts`),
 * else the closest shipped language to the operating system's preferred ones
 * ({@link detectLocale}). Sign-out's sweep clears the choice like every `el.`
 * key, so the next person at a shared machine gets the system's language.
 *
 * Right-to-left languages set `dir="rtl"` on the document; the theme and the
 * style cache follow it (`app/providers/BrandThemeProvider.tsx`).
 *
 * `t()` is a plain function, not a hook, so nothing re-renders when the
 * language changes. A caller applies a new choice by saving it and reloading
 * the page, and an entry applies the language before it imports the modules
 * that call `t()` at their top level.
 */
import { createStorage } from '@/shared/lib/storage';

import { DEFAULT_LOCALE, DEFAULT_NAMESPACE, i18n } from './i18n';

/** Each language in its own name. A language with a region (`fr-CA`) follows its plain one (`fr`): {@link detectLocale} prefers the earlier. */
export const LOCALES = [
  { code: 'en', label: 'English' },
  { code: 'de', label: 'Deutsch' },
  { code: 'es', label: 'Español' },
  { code: 'fr', label: 'Français' },
  { code: 'fr-CA', label: 'Français (Canada)' },
  { code: 'it', label: 'Italiano' },
  { code: 'nl', label: 'Nederlands' },
  { code: 'pl', label: 'Polski' },
  { code: 'pt-BR', label: 'Português (Brasil)' },
  { code: 'ru', label: 'Русский' },
  { code: 'uk', label: 'Українська' },
  { code: 'ja', label: '日本語' },
  { code: 'ko', label: '한국어' },
  { code: 'zh-CN', label: '简体中文' },
  { code: 'zh-TW', label: '繁體中文' },
  { code: 'ar', label: 'العربية', rtl: true },
  { code: 'he', label: 'עברית', rtl: true },
  { code: 'fa', label: 'فارسی', rtl: true },
  { code: 'ur', label: 'اردو', rtl: true },
] as const;

export type Locale = (typeof LOCALES)[number]['code'];
type Catalogue = Record<string, string>;

const LOCALE_STORAGE_KEY = 'lang';

/** Every catalogue file in the app, by path; `en` ones are already bundled. */
const CATALOGUES = import.meta.glob<Catalogue>('../../**/i18n/*.json', { import: 'default' });

/** `fr` owns `fr.json` and `fr.desktop.json`, never `fr-CA.json`. */
function cataloguesOf(locale: Locale): Array<() => Promise<Catalogue>> {
  return Object.entries(CATALOGUES)
    .filter(([path]) => {
      const name = path.slice(path.lastIndexOf('/') + 1);
      return name === `${locale}.json` || name.startsWith(`${locale}.`);
    })
    .map(([, load]) => load);
}

function isLocale(value: unknown): value is Locale {
  return LOCALES.some(({ code }) => code === value);
}

export function isRtl(locale: Locale): boolean {
  return LOCALES.some((entry) => entry.code === locale && 'rtl' in entry);
}

function maximized(tag: string): Intl.Locale | undefined {
  try {
    return new Intl.Locale(tag).maximize();
  } catch {
    return undefined;
  }
}

/**
 * The shipped language closest to the first of `languages` (the system's
 * preferred ones, `navigator.languages`) that has one: same language and
 * script (`zh-HK` reads Traditional Chinese, `zh-SG` Simplified), the same
 * region when one matches (`fr-CA`), else the plain one (`fr-BE` → `fr`).
 */
export function detectLocale(languages: readonly string[]): Locale {
  for (const language of languages) {
    const source = maximized(language);
    if (source === undefined) continue;
    const candidates = LOCALES.filter(({ code }) => {
      const target = maximized(code);
      return target?.language === source.language && target.script === source.script;
    });
    const match = candidates.find(({ code }) => maximized(code)?.region === source.region) ?? candidates[0];
    if (match !== undefined) return match.code;
  }
  return DEFAULT_LOCALE;
}

/** The person's own choice, if they made one on this device. */
function storedLocale(): Locale | undefined {
  try {
    const value = createStorage('local').get(LOCALE_STORAGE_KEY);
    return isLocale(value) ? value : undefined;
  } catch {
    return undefined;
  }
}

/** The language to start in: the person's choice, else the system's. */
export function startLocale(): Locale {
  return storedLocale() ?? detectLocale(typeof navigator === 'undefined' ? [] : navigator.languages);
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

/**
 * Loads `locale`'s catalogues, makes it the language `t()` answers in, and
 * sets the document's `lang` and `dir`. A catalogue that fails to load leaves
 * its keys in English.
 */
export async function applyLocale(locale: Locale): Promise<void> {
  if (locale !== DEFAULT_LOCALE) {
    const loaded = await Promise.allSettled(cataloguesOf(locale).map((load) => load()));
    for (const result of loaded) {
      if (result.status === 'fulfilled') i18n.addResourceBundle(locale, DEFAULT_NAMESPACE, result.value, true, false);
    }
  }
  await i18n.changeLanguage(locale);
  document.documentElement.lang = locale;
  document.documentElement.dir = isRtl(locale) ? 'rtl' : 'ltr';
}

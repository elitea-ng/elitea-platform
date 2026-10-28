/**
 * Every translated catalogue against its English source, found the way the
 * loader finds them (`shared/i18n/locales.ts`). A translation may lack a key
 * (it falls back to English), but it may not carry a key English no longer
 * has, and every `{{placeholder}}` and `<0>` tag must survive translation
 * byte for byte — a lost placeholder renders a blank, a translated one
 * renders literally. Plus language detection and switching.
 */
import { afterEach, describe, expect, it } from 'vitest';

import { applyLocale, detectLocale, i18n, LOCALES, t } from '@/shared/i18n';

import { registerDesktopCatalogue } from './registerDesktopCatalogue';

type Catalogue = Record<string, string>;

const FILES = import.meta.glob<Catalogue>('../../../**/i18n/*.json', { import: 'default', eager: true });

function split(path: string): { dir: string; locale: string; rest: string } {
  const dir = path.slice(0, path.lastIndexOf('/'));
  const name = path.slice(path.lastIndexOf('/') + 1);
  const dot = name.indexOf('.');
  return { dir, locale: name.slice(0, dot), rest: name.slice(dot) };
}

const TRANSLATIONS = Object.keys(FILES)
  .map((path) => ({ path, ...split(path) }))
  .filter(({ locale }) => locale !== 'en')
  .map(({ path, dir, locale, rest }) => [path, locale, `${dir}/en${rest}`] as const);

function tokens(value: string): string {
  return [...value.matchAll(/\{\{\s*[^}]+?\s*\}\}|<\/?[0-9a-zA-Z]+\s*\/?>/g)]
    .map((match) => match[0].replace(/\s+/g, ''))
    .sort()
    .join('|');
}

describe('translated catalogues', () => {
  it('exist only for shipped languages, beside an English one', () => {
    const shipped = new Set<string>(LOCALES.map(({ code }) => code));
    expect(TRANSLATIONS.length).toBeGreaterThan(0);
    for (const [path, locale, source] of TRANSLATIONS) {
      expect(shipped.has(locale), path).toBe(true);
      expect(FILES[source], path).toBeDefined();
    }
  });

  it.each(TRANSLATIONS)('%s carries no stale key and keeps every placeholder', (path, _locale, source) => {
    const en = FILES[source] ?? {};
    const translated = FILES[path] ?? {};
    expect(Object.keys(translated).filter((key) => !Object.hasOwn(en, key))).toEqual([]);
    const broken = Object.entries(translated).filter(([key, value]) => typeof value !== 'string' || tokens(value) !== tokens(en[key] ?? ''));
    expect(broken.map(([key]) => key)).toEqual([]);
  });
});

describe('detectLocale', () => {
  it.each([
    [['fr-CA'], 'fr-CA'],
    [['fr-BE'], 'fr'],
    [['fr'], 'fr'],
    [['de-AT', 'en'], 'de'],
    [['pt-PT'], 'pt-BR'],
    [['zh-HK'], 'zh-TW'],
    [['zh-TW'], 'zh-TW'],
    [['zh-SG'], 'zh-CN'],
    [['zh'], 'zh-CN'],
    [['es-MX'], 'es'],
    [['sw-KE', 'he-IL'], 'he'],
    [['sw-KE'], 'en'],
    [['not a tag', 'ja-JP'], 'ja'],
    [[], 'en'],
  ] as const)('%j → %s', (languages, expected) => {
    expect(detectLocale(languages)).toBe(expected);
  });
});

describe('applyLocale', () => {
  afterEach(async () => {
    await applyLocale('en');
  });

  it('answers in the chosen language, desktop strings included, and English for a key it lacks', async () => {
    registerDesktopCatalogue();
    await applyLocale('fr-CA');
    expect(i18n.language).toBe('fr-CA');
    expect(document.documentElement.lang).toBe('fr-CA');
    expect(document.documentElement.dir).toBe('ltr');
    expect(t('desktop.account.signOut', 'Sign out')).not.toBe('Sign out');
    expect(t('widgets.sidebar.nav.agents', 'Agents')).not.toBe('');
    i18n.addResourceBundle('en', 'translation', { 'test.onlyInEnglish': 'Only in English' }, true, false);
    expect(t('test.onlyInEnglish', 'fallback')).toBe('Only in English');
  });

  it('turns the document right-to-left for a right-to-left language, and back', async () => {
    await applyLocale('ar');
    expect(document.documentElement.dir).toBe('rtl');
    await applyLocale('en');
    expect(document.documentElement.dir).toBe('ltr');
  });
});

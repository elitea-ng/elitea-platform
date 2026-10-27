/**
 * The translated catalogues against their English sources. A translation
 * may lack a key (it falls back to English), but it may not carry a key
 * English no longer has, and every `{{placeholder}}` and `<0>` tag must
 * survive translation byte for byte — a lost placeholder renders a blank,
 * a translated one renders literally.
 */
import { afterEach, describe, expect, it } from 'vitest';

import { applyLocale, i18n, t } from '@/shared/i18n';

import enChatCloud from '../../../pages/chat/i18n/en.chatCloud.json';
import esChatCloud from '../../../pages/chat/i18n/es.chatCloud.json';
import frChatCloud from '../../../pages/chat/i18n/fr-CA.chatCloud.json';
import enCloudSwitch from '../../../features/cloud-executor-switch/i18n/en.cloudExecutorSwitch.json';
import esCloudSwitch from '../../../features/cloud-executor-switch/i18n/es.cloudExecutorSwitch.json';
import frCloudSwitch from '../../../features/cloud-executor-switch/i18n/fr-CA.cloudExecutorSwitch.json';
import enForge from '../../../features/forge-credential/i18n/en.forgeCredential.json';
import esForge from '../../../features/forge-credential/i18n/es.forgeCredential.json';
import frForge from '../../../features/forge-credential/i18n/fr-CA.forgeCredential.json';
import enShared from '../../../shared/i18n/en.json';
import esShared from '../../../shared/i18n/es.json';
import frShared from '../../../shared/i18n/fr-CA.json';
import enDesktop from './en.desktop.json';
import esDesktop from './es.desktop.json';
import frDesktop from './fr-CA.desktop.json';
import { registerDesktopCatalogue } from './registerDesktopCatalogue';

type Catalogue = Record<string, string>;

const PAIRS: readonly [string, Catalogue, Catalogue, Catalogue][] = [
  ['en.json', enShared, frShared, esShared],
  ['en.desktop.json', enDesktop, frDesktop, esDesktop],
  ['en.chatCloud.json', enChatCloud, frChatCloud, esChatCloud],
  ['en.cloudExecutorSwitch.json', enCloudSwitch, frCloudSwitch, esCloudSwitch],
  ['en.forgeCredential.json', enForge, frForge, esForge],
];

function tokens(value: string): string {
  return [...value.matchAll(/\{\{\s*[^}]+?\s*\}\}|<\/?[0-9a-zA-Z]+\s*\/?>/g)]
    .map((match) => match[0].replace(/\s+/g, ''))
    .sort()
    .join('|');
}

describe.each(PAIRS)('%s translations', (_name, en, frCa, es) => {
  it.each([
    ['fr-CA', frCa],
    ['es', es],
  ] as const)('%s carries no stale key and keeps every placeholder', (_locale, translated) => {
    expect(Object.keys(translated).filter((key) => !Object.hasOwn(en, key))).toEqual([]);
    const broken = Object.entries(translated).filter(([key, value]) => typeof value !== 'string' || tokens(value) !== tokens(en[key] ?? ''));
    expect(broken.map(([key]) => key)).toEqual([]);
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
    expect(t('desktop.account.signOut', 'Sign out')).toBe(frDesktop['desktop.account.signOut']);
    expect(t('widgets.sidebar.nav.agents', 'Agents')).toBe(frShared['widgets.sidebar.nav.agents']);
    i18n.addResourceBundle('en', 'translation', { 'test.onlyInEnglish': 'Only in English' }, true, false);
    expect(t('test.onlyInEnglish', 'fallback')).toBe('Only in English');
  });
});

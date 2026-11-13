import { afterEach, describe, expect, it } from 'vitest';

import { applyLocale, i18n, saveLocale, t } from '@/shared/i18n';

import { applyStartLocale } from './applyStartLocale';

afterEach(async () => {
  window.localStorage.clear();
  await applyLocale('en');
});

describe('applyStartLocale', () => {
  it('starts in the language the account menu saved, so the web UI leaves English', async () => {
    saveLocale('de');
    await applyStartLocale();
    expect(i18n.language).toBe('de');
    expect(document.documentElement.lang).toBe('de');
    // A key with a German catalogue entry no longer answers in English.
    expect(t('settings.general', 'General')).not.toBe('General');
  });

  it('stays in English when nothing was chosen and the system asks for English', async () => {
    await applyStartLocale();
    expect(i18n.language).toBe('en');
  });
});

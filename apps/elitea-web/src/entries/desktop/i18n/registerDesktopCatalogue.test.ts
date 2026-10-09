import { describe, expect, it } from 'vitest';

import { i18n, t } from '@/shared/i18n';

import catalogue from './en.desktop.json';
import { registerDesktopCatalogue } from './registerDesktopCatalogue';
import shared from '../../../shared/i18n/en.json';

describe('registerDesktopCatalogue', () => {
  it('keeps the desktop strings out of the shared catalogue', () => {
    const desktopKeys = Object.keys(catalogue);
    expect(desktopKeys.length).toBeGreaterThan(50);
    expect(desktopKeys.filter((key) => Object.hasOwn(shared, key))).toEqual([]);
  });

  it('makes every desktop key resolve through t() once registered', () => {
    registerDesktopCatalogue();
    expect(i18n.exists('workspace.openFolder')).toBe(true);
    expect(t('workspace.openFolder', 'fallback-not-used')).toBe(catalogue['workspace.openFolder']);
    // Shared strings are untouched.
    expect(t('workspace.desktopOnly', 'x')).toBe(shared['workspace.desktopOnly']);
  });
});

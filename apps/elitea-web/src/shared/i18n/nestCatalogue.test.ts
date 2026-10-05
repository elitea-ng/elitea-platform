/**
 * `nestCatalogue` changes the catalogue's shape, never what a key says: the
 * built app and admin console look every key up in the nested tree, while
 * unit tests (vitest has its own plugin list) still read the flat file.
 */
import i18next from 'i18next';
import { describe, expect, it } from 'vitest';

import en from './en.json';
import { nestCatalogue } from './nestCatalogue';

async function instanceOver(resource: object) {
  const instance = i18next.createInstance();
  await instance.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: { en: { translation: resource } },
    interpolation: { escapeValue: false },
    returnEmptyString: false,
  });
  return instance;
}

describe('nestCatalogue', () => {
  it('folds dotted keys into a tree', () => {
    expect(nestCatalogue({ 'a.b.c': 'x', 'a.b.d': 'y', 'a.e': 'z' })).toEqual({ a: { b: { c: 'x', d: 'y' }, e: 'z' } });
  });

  it('keeps a key flat when a shorter key already holds a string on its path', () => {
    expect(nestCatalogue({ 'a.b.c': 'long', 'a.b': 'short', 'a.b.c.d': 'longer' })).toEqual({
      a: { b: 'short' },
      'a.b.c': 'long',
      'a.b.c.d': 'longer',
    });
  });

  it('keeps a key with an empty segment flat', () => {
    expect(nestCatalogue({ 'a..b': '1', '.a': '2', 'a.': '3', a: '4' })).toEqual({ 'a..b': '1', '.a': '2', 'a.': '3', a: '4' });
  });

  it('writes a __proto__ segment as an own key, not the prototype', () => {
    const tree = nestCatalogue({ 'x.__proto__.y': 'v' });
    expect(Object.getPrototypeOf(tree.x)).toBe(Object.prototype);
    expect(JSON.parse(JSON.stringify(tree))).toEqual(JSON.parse('{"x":{"__proto__":{"y":"v"}}}'));
  });

  it('resolves every key in en.json to its own string through i18next', async () => {
    const flat = en as Record<string, string>;
    const instance = await instanceOver(JSON.parse(JSON.stringify(nestCatalogue(flat))) as object);
    const wrong = Object.keys(flat).filter((key) => instance.t(key, { defaultValue: '\u0000missing', interpolation: { skipOnVariables: true }, skipInterpolation: true }) !== flat[key]);
    expect(wrong).toEqual([]);
  });

  it('is smaller than the flat catalogue', () => {
    expect(JSON.stringify(nestCatalogue(en as Record<string, string>)).length).toBeLessThan(JSON.stringify(en).length);
  });
});

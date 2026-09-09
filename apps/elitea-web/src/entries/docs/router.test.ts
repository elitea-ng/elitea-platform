/**
 * `resolveContentHref`/`withBase`/`isExternalHref` — the fix for content
 * links written as root-relative slugs (`README.md`'s convention,
 * `[Voice](/menus/chat#voice)`) needing the docs base (`import.meta.env
 * .BASE_URL`, set from `DOCS_BASE` at build time) prefixed before they reach
 * the DOM. Tests read `BASE_URL` from the same module rather than assuming
 * a literal value — under `vitest.config.ts`'s shared config (not the
 * `docs` Vite mode) it is `/`, not `/docs/`, so asserting against a
 * hardcoded `/docs/…` would pass for the wrong reason.
 */
import { describe, expect, it } from 'vitest';

import { isExternalHref, resolveContentHref, toPath, withBase } from './router';

const BASE = import.meta.env.BASE_URL;

describe('isExternalHref', () => {
  it.each([
    ['https://example.com', true],
    ['http://example.com', true],
    ['HTTPS://Example.com', true],
    ['mailto:support@elitea.ai', true],
    ['/menus/chat', false],
    ['/menus/chat#voice', false],
    ['#voice', false],
    ['', false],
  ])('%s -> %s', (href, expected) => {
    expect(isExternalHref(href)).toBe(expected);
  });
});

describe('withBase', () => {
  it('prefixes a root-relative path with BASE', () => {
    expect(withBase('/menus/chat')).toBe(`${BASE}menus/chat`);
  });

  it('is idempotent: a path already under BASE is left alone', () => {
    const already = toPath('menus/chat');
    expect(withBase(already)).toBe(already);
  });

  it('handles the bare-root case', () => {
    expect(withBase('/')).toBe(BASE);
  });

  it('carries a hash fragment through untouched', () => {
    expect(withBase('/menus/chat#voice')).toBe(`${BASE}menus/chat#voice`);
  });
});

describe('resolveContentHref', () => {
  it('rewrites a root-relative slug to live under BASE', () => {
    expect(resolveContentHref('/menus/chat')).toBe(`${BASE}menus/chat`);
  });

  it('rewrites a root-relative slug with a heading anchor', () => {
    expect(resolveContentHref('/menus/chat#voice')).toBe(`${BASE}menus/chat#voice`);
  });

  it('leaves a bare #anchor untouched (native in-page scroll)', () => {
    expect(resolveContentHref('#voice')).toBe('#voice');
  });

  it('leaves an https:// link untouched', () => {
    expect(resolveContentHref('https://github.com/elitea-ng/elitea-platform/issues/1')).toBe(
      'https://github.com/elitea-ng/elitea-platform/issues/1',
    );
  });

  it('leaves a mailto: link untouched', () => {
    expect(resolveContentHref('mailto:support@elitea.ai')).toBe('mailto:support@elitea.ai');
  });

  it('does not double-prefix an href a caller already based', () => {
    const already = toPath('menus/chat');
    expect(resolveContentHref(already)).toBe(already);
  });

  it('passes through a non-root-relative, non-hash, non-external value unchanged', () => {
    expect(resolveContentHref('menus/chat')).toBe('menus/chat');
  });
});

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

import { renderBrandArtifacts } from '../../scripts/gen-brand.mjs';

import { BRAND_TOKENS, DEFAULT_BRAND_HUE, DEFAULT_PACK_ACCENTS, DEFAULT_PRODUCT, ELITEA_MARK_ASPECT, ELITEA_MARK_SVG, ELITEA_MARK_VIEWBOX } from './index';

const read = (name: string): string => readFileSync(new URL(name, import.meta.url), 'utf8');

describe('brand defaults', () => {
  it('the mark string is the canonical SVG', () => {
    assert.equal(ELITEA_MARK_SVG, read('./logo-mark.svg').trim());
    assert.ok(ELITEA_MARK_SVG.startsWith(`<svg viewBox="${ELITEA_MARK_VIEWBOX}"`));
    assert.equal(ELITEA_MARK_ASPECT, 1);
  });

  it('the pack accents carry the identity hue', () => {
    assert.equal(DEFAULT_PACK_ACCENTS.hue, DEFAULT_BRAND_HUE);
    assert.equal(DEFAULT_PRODUCT.name, 'Elitea AI');
    assert.equal(DEFAULT_PRODUCT.name, BRAND_TOKENS.name);
    assert.equal(DEFAULT_BRAND_HUE.toUpperCase(), BRAND_TOKENS.palette.indigo.value);
  });
});

describe('renderBrandArtifacts', () => {
  const mark = '<svg viewBox="0 0 372 372" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M0 0"></path></svg>';
  const byPath = (svg: string) => new Map(renderBrandArtifacts(svg, '<svg/>').map((a: { path: string; content: string }) => [a.path, a.content]));

  it('sizes the web copy, squares the favicon and keeps the body', () => {
    const out = byPath(mark);
    assert.equal(
      out.get('apps/elitea-web/public/brand/logo-mark.svg'),
      '<svg viewBox="0 0 372 372" width="100%" height="100%" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M0 0"></path></svg>\n',
    );
    assert.equal(
      out.get('apps/elitea-web/public/brand/favicon.svg'),
      '<svg viewBox="0 0 512 512" width="512" height="512" fill="none" xmlns="http://www.w3.org/2000/svg"><rect width="512" height="512" rx="114" fill="#0B0B18"/><g transform="translate(70 70)"><path d="M0 0"></path></g></svg>\n',
    );
    assert.equal(out.get('apps/elitea-web/public/brand/logo-full.svg'), '<svg/>\n');
    assert.match(out.get('libs/ts/client-core/src/brand/mark.gen.ts') ?? '', /export const ELITEA_MARK_ASPECT = 372 \/ 372;/);
  });

  for (const [why, svg] of [
    ['a sized root', mark.replace('fill="none"', 'width="10" fill="none"')],
    ['another viewBox', mark.replace('0 0 372 372', '0 0 10 10')],
    ['a single quote', mark.replace('M0 0', "M0 0'")],
    ['no svg root', '<g/>'],
  ] as const) {
    it(`refuses ${why}`, () => {
      assert.throws(() => renderBrandArtifacts(svg, '<svg/>'));
    });
  }
});

// The web build's brand (ADR-0052 W3.1): scripts/brand-web.mjs over both brands.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { BRANDS_DIR, loadBrandFrom } from '../../../scripts/brand/load.mjs';
import { BrandPack } from '../src/shared/brand/schema';
import { brandDefaultPack, brandLogoFiles, brandTitle, fillSized, isDefaultBrand } from './brand-web.mjs';

const APP = join(import.meta.dirname, '..');
const reference = JSON.parse(readFileSync(join(APP, 'src/shared/brand/tokens/default.pack.json'), 'utf8'));
const brand = (id) => loadBrandFrom(join(BRANDS_DIR, id));

describe('the default brand is the committed build', () => {
  it('brand/elitea needs no generated file', () => {
    expect(isDefaultBrand(brand('elitea'))).toBe(true);
    expect(isDefaultBrand(brand('sample'))).toBe(false);
  });

  it("the committed logo copies are brand/elitea's, sized as the build writes them", () => {
    const logos = brandLogoFiles(brand('elitea'));
    for (const [name, content] of Object.entries(logos.public))
      expect(readFileSync(join(APP, 'public/brand', name), 'utf8')).toBe(content);
    for (const [name, content] of Object.entries(logos.icons))
      expect(readFileSync(join(APP, 'src/shared/ui/icons/svg', name), 'utf8')).toBe(content);
  });

  it("brand/elitea's manifest derives the committed pack's product fields", () => {
    const derived = brandDefaultPack(brand('elitea'), reference);
    for (const field of ['name', 'shortName', 'docsUrl']) expect(derived.product[field]).toBe(reference.product[field]);
    expect(derived.brand.hue.toLowerCase()).toBe(reference.brand.hue.toLowerCase());
  });

  it('titles are unchanged for ELITEA', () => {
    for (const title of ['Elitea AI', 'Elitea AI Admin', 'Elitea — Maintenance', 'Brand preview']) {
      const html = `<head><title>${title}</title></head>`;
      expect(brandTitle(html, brand('elitea'), reference)).toBe(html);
    }
  });
});

describe('brand/sample builds its own default pack, logos and titles', () => {
  const pack = brandDefaultPack(brand('sample'), reference);

  it('the derived pack is a valid brand pack with the manifest product and hue', () => {
    const parsed = BrandPack.parse(pack);
    expect(parsed.id).toBe('sample');
    expect(parsed.product).toMatchObject({
      name: 'Sample Assistant',
      shortName: 'Sample',
      supportEmail: 'support@sample.example.com',
      supportUrl: 'https://sample.example.com/support',
    });
    expect(parsed.brand.hue).toBe('#0e7490');
  });

  it("drops ELITEA's token values, so every token derives from the brand hue", () => {
    expect(pack.schemes).toEqual({ light: {}, dark: {} });
  });

  it('names no ELITEA product, address or colour', () => {
    // The `$schema` URL is the pack format's id (BrandPack requires it), not a name.
    const { $schema, ...rest } = pack;
    expect($schema).toBe(reference.$schema);
    const text = JSON.stringify(rest);
    expect(text).not.toMatch(/elitea|epam/i);
    expect(text).not.toContain(reference.brand.hue);
  });

  it('a manifest without a support address states none', () => {
    const b = brand('sample');
    const without = { ...b, manifest: { ...b.manifest, supportEmail: null } };
    expect(brandDefaultPack(without, reference).product).not.toHaveProperty('supportEmail');
  });

  it("a brand folder's pack.json is used as it is", () => {
    const b = brand('sample');
    const own = {
      ...b,
      has: (rel) => rel === 'pack.json' || b.has(rel),
      readText: (rel) => (rel === 'pack.json' ? '{"id":"own"}' : b.readText(rel)),
    };
    expect(brandDefaultPack(own, reference)).toEqual({ id: 'own' });
  });

  it("the logos are brand/sample's", () => {
    const logos = brandLogoFiles(brand('sample'));
    expect(logos.public['favicon.svg']).toBe(`${brand('sample').readText('assets/web/favicon.svg').trimEnd()}\n`);
    expect(logos.icons['logo-mark-icon.svg']).toContain('width="100%" height="100%"');
    expect(Object.values(logos.public).join('')).not.toMatch(/elitea/i);
  });

  it('titles take the product and short names', () => {
    const b = brand('sample');
    expect(brandTitle('<title>Elitea AI</title>', b, reference)).toBe('<title>Sample Assistant</title>');
    expect(brandTitle('<title>Elitea AI Admin</title>', b, reference)).toBe('<title>Sample Assistant Admin</title>');
    expect(brandTitle('<title>Elitea — Maintenance</title>', b, reference)).toBe('<title>Sample — Maintenance</title>');
    expect(brandTitle('<title>Brand preview</title>', b, reference)).toBe('<title>Brand preview</title>');
  });
});

describe('fillSized', () => {
  it('sets width and height to 100% on the root, replacing any', () => {
    expect(fillSized('<svg viewBox="0 0 1 1" width="5"><g/></svg>')).toBe(
      '<svg viewBox="0 0 1 1" width="100%" height="100%"><g/></svg>\n',
    );
    expect(fillSized('<svg xmlns="x"><g/></svg>')).toBe('<svg width="100%" height="100%" xmlns="x"><g/></svg>\n');
    expect(() => fillSized('<g/>')).toThrow(/not an SVG/);
  });
});

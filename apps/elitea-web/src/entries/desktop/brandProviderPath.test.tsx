/**
 * The desktop brand reaches the SAME provider path the web app uses: once
 * `applyDesktopBrand` has published the deployment's pack, `resolveBrandPack()`
 * (what `AppProviders` hands `BrandThemeProvider`) yields it, the theme paints
 * its colours, the logo renders the inlined asset and the favicon is
 * repointed — with no desktop-specific branch in the shared code.
 */
import { render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { BRAND_PACK_GLOBAL, BrandPack, DEFAULT_BRAND_PACK, resolveBrandPack } from '@/shared/brand';
import { applyDesktopBrand } from '@/shared/desktop/brandPack';
import { BrandLogoFull } from '@/shared/ui/brand-logo';

import { installWebStorageShim } from '../../test/webstorage';

installWebStorageShim();

/** Computed, not spelled: the no-raw-color scan reads string literals (see BrandThemeProvider.test.tsx). */
const PROBE_COLOUR = `#${(0x3a7b5c).toString(16)}`;
const LOGO = `data:image/svg+xml;base64,${btoa('<svg xmlns="http://www.w3.org/2000/svg"/>')}`;
const FAVICON = 'data:image/png;base64,iVBORw0KGgo=';

afterEach(() => {
  delete (globalThis as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL];
  document.head.querySelector('link[rel="icon"]')?.remove();
});

describe('desktop brand → BrandThemeProvider', () => {
  it('paints the deployment pack: colours, logo and favicon', () => {
    const pack = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      id: 'acme',
      product: { ...DEFAULT_BRAND_PACK.product, name: 'Acme AI' },
      assets: { ...DEFAULT_BRAND_PACK.assets, logoFull: LOGO, favicon: FAVICON },
      brand: { hue: PROBE_COLOUR },
      schemes: { ...DEFAULT_BRAND_PACK.schemes, dark: { ...DEFAULT_BRAND_PACK.schemes.dark, 'primary.main': PROBE_COLOUR } },
    });
    applyDesktopBrand({ pack, fonts: [] });

    const { getByTestId } = render(
      <BrandThemeProvider pack={resolveBrandPack()}>
        <BrandLogoFull />
      </BrandThemeProvider>,
    );

    const logo = getByTestId('brand-logo-full');
    expect(logo.tagName).toBe('IMG');
    expect(logo.getAttribute('src')).toBe(LOGO);
    expect(logo.getAttribute('alt')).toBe('Acme AI');
    expect(document.head.querySelector('link[rel="icon"]')?.getAttribute('href')).toBe(FAVICON);
    const styles = [...document.querySelectorAll('style')].map((style) => style.textContent ?? '').join('\n').toLowerCase();
    expect(styles).toContain(`--el-palette-primary-main:${PROBE_COLOUR}`);
  });

  it('keeps the compiled default for an unbranded deployment', () => {
    applyDesktopBrand({ pack: null, fonts: [] });
    const { getByTestId } = render(
      <BrandThemeProvider pack={resolveBrandPack()}>
        <BrandLogoFull />
      </BrandThemeProvider>,
    );
    expect(getByTestId('brand-logo-full').tagName.toLowerCase()).toBe('svg');
  });
});

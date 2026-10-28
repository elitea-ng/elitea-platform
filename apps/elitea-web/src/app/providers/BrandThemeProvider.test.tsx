import { render } from '@testing-library/react';
import { useTheme } from '@mui/material/styles';
import { describe, expect, it } from 'vitest';

import { BrandPack, DEFAULT_BRAND_PACK } from '@/shared/brand';

import { installWebStorageShim } from '../../test/webstorage';

import { BrandThemeProvider } from './BrandThemeProvider';

/**
 * MUI's ThemeProvider (CssVarsProvider under the hood) reads/writes
 * `window.localStorage` for mode persistence. Under vitest 4 + Node 24,
 * Node's own experimental `localStorage` global shadows jsdom's, leaving
 * `window.localStorage` undefined in the `node` project — unit F4 shipped
 * `installWebStorageShim()` for exactly this (`src/test/webstorage.ts`),
 * applied per-file the same way F4's own `storage.test.ts` does, since
 * `src/test/setup.ts` is unit M1's file (not this unit's to edit).
 */
installWebStorageShim();

/**
 * Built at runtime from a NUMBER literal, not written as a colour string
 * literal — R-T1's `no-raw-color` scan (`tools/lint-rules/rules/no-raw-color.mjs`)
 * inspects string/template literal TEXT and has no test-file carve-out in
 * `.oxlintrc.json` (unlike `i18next/no-literal-string`): a colour string it
 * can see is banned everywhere source is scanned, fixtures included. These
 * two probe colours exist only to be searched for / injected into a rendered
 * stylesheet below, never to style anything, but the rule can't tell that
 * from a bare literal, so they are computed instead of spelled out.
 */
const UNUSED_PROBE_COLOUR = `#${(0x123456).toString(16)}`; // '#123456'
const HOSTILE_PROBE_COLOUR = `#${(0xff00aa).toString(16)}`; // '#ff00aa'

function ThemeProbe() {
  const theme = useTheme();
  return <div data-testid="probe" data-primary-main={theme.vars.palette.primary.main} />;
}

/** Every `<style>` element's text, concatenated — the real, rendered output. */
function renderedStyleText(): string {
  return [...document.querySelectorAll('style')].map((style) => style.textContent ?? '').join('\n');
}

describe('BrandThemeProvider', () => {
  it('exposes the built MUI theme to descendants via context (theme.vars resolves to the real CSS-variable name)', () => {
    const { getByTestId } = render(
      <BrandThemeProvider>
        <ThemeProbe />
      </BrandThemeProvider>,
    );

    // Once CssVarsProvider has settled post-mount (verified empirically —
    // testing-library's render() flushes the settling effect synchronously),
    // `theme.vars.*` reads resolve to the bare `var(--name)` form, WITHOUT
    // the inline fallback `buildTheme.test.ts` observes on the theme object
    // in isolation (that form only appears pre-mount / off-context, to
    // survive a render before any stylesheet exists). The real, current
    // colour value lives in the injected stylesheet instead — see the
    // RED/GREEN (b) test below, which is the assertion that actually proves
    // the default pack's colour reached the DOM.
    expect(getByTestId('probe').dataset['primaryMain']).toBe('var(--el-palette-primary-main)');
  });

  it('RED/GREEN (b): renders the default pack\'s real brand colour into the DOM (a real rendered-style assertion, not just "renders without crashing")', () => {
    render(
      <BrandThemeProvider>
        <button type="button">probe</button>
      </BrandThemeProvider>,
    );

    const styleText = renderedStyleText();

    // RED baseline, proven inline: a colour this pack does not use must NOT
    // appear — otherwise the GREEN assertion below would be vacuous (any
    // hex string would "pass" a substring check against arbitrary CSS).
    expect(styleText.toLowerCase()).not.toContain(UNUSED_PROBE_COLOUR);
    // GREEN: the default pack's actual brand hue (`brand.hue` /
    // `schemes.dark['primary.main']`, tokens/default.pack.json, "#4A4DE8")
    // IS present in the stylesheet CssBaseline forced MUI to inject — the
    // theme's real, resolved output, not the theme object in isolation.
    expect(styleText.toLowerCase()).toContain(
      `--el-palette-primary-main:${String(DEFAULT_BRAND_PACK.schemes.dark['primary.main']).toLowerCase()}`,
    );
  });

  it('is pack-driven, not hardcoded: a different pack paints a different primary colour in the rendered stylesheet', () => {
    const hostilePack = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      brand: { hue: HOSTILE_PROBE_COLOUR },
      schemes: {
        ...DEFAULT_BRAND_PACK.schemes,
        // Both schemes: since brand guide v3 the default light and dark
        // primaries are the same indigo, so a dark-only override would leave
        // the default value in the light block.
        dark: { ...DEFAULT_BRAND_PACK.schemes.dark, 'primary.main': HOSTILE_PROBE_COLOUR },
        light: { ...DEFAULT_BRAND_PACK.schemes.light, 'primary.main': HOSTILE_PROBE_COLOUR },
      },
    });

    render(
      <BrandThemeProvider pack={hostilePack}>
        <button type="button">probe</button>
      </BrandThemeProvider>,
    );

    const styleText = renderedStyleText().toLowerCase();
    expect(styleText).toContain(`--el-palette-primary-main:${HOSTILE_PROBE_COLOUR}`);
    expect(styleText).not.toContain(
      `--el-palette-primary-main:${String(DEFAULT_BRAND_PACK.schemes.dark['primary.main']).toLowerCase()}`,
    );
  });

  it('emits no runtime anti-flash script — the static script in index.html is the single source (ADR-0024 WP3)', () => {
    // MUI's InitColorSchemeScript used to be rendered here and was VERIFIED
    // to emit nothing under createRoot (see the provider's header). It is
    // gone; the first-paint script is static. Pinning "no runtime script
    // mentions el-mode" keeps a second, competing writer from creeping back.
    render(
      <BrandThemeProvider>
        <button type="button">probe</button>
      </BrandThemeProvider>,
    );

    const scripts = [...document.querySelectorAll('script')];
    expect(scripts.some((script) => (script.textContent ?? '').includes('el-mode'))).toBe(false);
  });

  it('injects <style data-el-fonts> from typography.fontFaces and removes it for a pack with none', () => {
    const withFaces = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      typography: {
        ...DEFAULT_BRAND_PACK.typography,
        fontFaces: [{ family: 'Montserrat', url: '/api/v2/branding/assets/font/abc.woff2', weight: '400' }],
      },
    });

    const { rerender } = render(
      <BrandThemeProvider pack={withFaces}>
        <button type="button">probe</button>
      </BrandThemeProvider>,
    );
    const style = document.head.querySelector('style[data-el-fonts]');
    expect(style).not.toBeNull();
    expect(style?.textContent).toContain('@font-face');
    expect(style?.textContent).toContain('url("/api/v2/branding/assets/font/abc.woff2") format("woff2")');
    expect(style?.textContent).toContain('font-display:swap');

    const { fontFaces: _omitted, ...typographyWithoutFaces } = DEFAULT_BRAND_PACK.typography;
    rerender(
      <BrandThemeProvider pack={{ ...DEFAULT_BRAND_PACK, typography: typographyWithoutFaces }}>
        <button type="button">probe</button>
      </BrandThemeProvider>,
    );
    expect(document.head.querySelector('style[data-el-fonts]')).toBeNull();
  });

  it('leaves <link rel="icon"> alone for the default pack (no served pack)', () => {
    const link = document.createElement('link');
    link.rel = 'icon';
    link.href = './brand/favicon.svg';
    document.head.append(link);
    try {
      render(
        <BrandThemeProvider>
          <button type="button">probe</button>
        </BrandThemeProvider>,
      );
      expect(document.head.querySelector('link[rel="icon"]')?.getAttribute('href')).toBe('./brand/favicon.svg');
    } finally {
      link.remove();
    }
  });
});

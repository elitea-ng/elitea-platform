import { describe, expect, it } from 'vitest';

import { BREAKPOINT_VALUES, buildEliteaTheme } from '../buildTheme';
import { COLOR_SCHEME_SELECTOR, CSS_VAR_PREFIX, DEFAULT_COLOR_SCHEME } from '../constants';
import { muiOverrides } from '../mui-overrides';
import { BrandPack } from '../schema';
import { catalogueFor } from '../toMuiPalette';
import { DEFAULT_BRAND_PACK } from '../tokens';
import { emittedCssVars } from './reference-scan';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

describe('motion', () => {
  it('follows the system reduced-motion setting for MUI transitions', () => {
    expect(theme.motion.reducedMotion).toBe('system');
  });
});

describe('CSS-variable wiring', () => {
  it('uses the el prefix and the data-attribute scheme selector', () => {
    expect(theme.cssVarPrefix).toBe(CSS_VAR_PREFIX);
    expect(theme.colorSchemeSelector).toBe(COLOR_SCHEME_SELECTOR);
    expect(theme.defaultColorScheme).toBe(DEFAULT_COLOR_SCHEME);
  });

  it('resolves %s into a real selector per scheme, with dark on :root', () => {
    const selectors = theme
      .generateStyleSheets()
      .flatMap((sheet) => Object.keys(sheet))
      .filter((key) => key.includes('data-el-scheme'));
    expect(selectors).toContain(':root, [data-el-scheme="dark"]');
    expect(selectors).toContain('[data-el-scheme="light"]');
  });

  it('emits a variable for every token id the default pack states', () => {
    const emitted = emittedCssVars(theme);
    const missing = catalogueFor('dark')
      .map((id) => `--${CSS_VAR_PREFIX}-palette-${id.split('.').join('-')}`)
      .filter((cssVar) => !emitted.has(cssVar));
    expect(missing).toEqual([]);
  });

  it('exposes the same tokens on theme.vars, with the value as a var fallback', () => {
    // MUI's `vars` are `varsWithDefaults`: `var(--name, <resolved value>)`.
    // The fallback is what makes an override still paint if the variable
    // stylesheet has not been injected yet.
    expect(theme.vars.palette.background.button.primary.default).toBe(
      `var(--el-palette-background-button-primary-default, ${DEFAULT_BRAND_PACK.schemes.dark['background.button.primary.default']})`,
    );
    expect(theme.vars.shape.radiusLg).toBe(
      `var(--el-shape-radiusLg, ${DEFAULT_BRAND_PACK.shape.radiusLg}px)`,
    );
    // [S1 Part B] radiusPill — the pill/circle escape hatch MuiButton's
    // icon-only and maxi variants use; see buildTheme.ts.
    expect(theme.vars.shape.radiusPill).toBe(
      `var(--el-shape-radiusPill, ${DEFAULT_BRAND_PACK.shape.radiusPill}px)`,
    );
  });
});

describe('shape, spacing and breakpoints', () => {
  it('maps radii from the pack, keeping borderRadius on radiusMd', () => {
    expect(theme.shape.borderRadius).toBe(DEFAULT_BRAND_PACK.shape.radiusMd);
    expect(theme.shape.radiusSm).toBe(DEFAULT_BRAND_PACK.shape.radiusSm);
    expect(theme.shape.radiusLg).toBe(DEFAULT_BRAND_PACK.shape.radiusLg);
    expect(theme.shape.radiusPill).toBe(DEFAULT_BRAND_PACK.shape.radiusPill);
  });

  it('treats density as a token: 8 comfortable, 6 compact', () => {
    expect(theme.spacing(1)).toBe('var(--el-spacing, 8px)');
    const compact = buildEliteaTheme(
      BrandPack.parse({
        ...DEFAULT_BRAND_PACK,
        shape: { ...DEFAULT_BRAND_PACK.shape, density: 'compact' },
      }),
    );
    expect(compact.spacing(1)).toBe('var(--el-spacing, 6px)');
  });

  it('carries the baseline breakpoints, custom rungs included', () => {
    for (const [name, value] of Object.entries(BREAKPOINT_VALUES)) {
      expect(theme.breakpoints.values[name as keyof typeof BREAKPOINT_VALUES]).toBe(value);
    }
    expect(theme.breakpoints.values.prompt_list_xxxxxl).toBe(5120);
  });
});

describe('component overrides', () => {
  it('wires exactly the keys the override package exports', () => {
    expect(Object.keys(theme.components ?? {})).toEqual(Object.keys(muiOverrides()));
  });

  it('paints every colour through a token, never a literal', () => {
    const COLOUR_KEYS = new Set(['color', 'background', 'backgroundColor']);
    const offences: string[] = [];
    const walk = (node: unknown, path: string): void => {
      if (node === null || typeof node !== 'object') return;
      for (const [key, value] of Object.entries(node as Record<string, unknown>)) {
        if (typeof value === 'string') {
          if (COLOUR_KEYS.has(key) && !value.startsWith('var(--el-palette-') && value !== 'transparent') {
            offences.push(`${path}.${key} = ${value}`);
          }
        } else {
          walk(value, `${path}.${key}`);
        }
      }
    };

    const styles = (muiOverrides().MuiButton?.variants ?? []).map((variant) =>
      typeof variant.style === 'function'
        ? (variant.style as (arg: unknown) => unknown)({ theme })
        : variant.style,
    );
    expect(styles.length).toBeGreaterThan(0);
    styles.forEach((style, index) => walk(style, `MuiButton[${index}]`));

    const chip = muiOverrides().MuiChip?.styleOverrides ?? {};
    for (const [slot, style] of Object.entries(chip)) {
      walk(typeof style === 'function' ? (style as (arg: unknown) => unknown)({ theme }) : style, `MuiChip.${slot}`);
    }
    expect(offences).toEqual([]);
  });
});

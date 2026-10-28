import { render, screen } from '@testing-library/react';
import { useTheme } from '@mui/material/styles';
import { useQueryClient } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BRAND_PACK_GLOBAL, DEFAULT_BRAND_PACK, resolveBrandPack } from '@/shared/brand';
import { formatHex, hslaToRgba } from '@/shared/brand/color';
import { i18n, t } from '@/shared/i18n';
import type { TFunction } from '@/shared/i18n';

import { installWebStorageShim } from '../../test/webstorage';

import { AppProviders } from './AppProviders';

installWebStorageShim();

/**
 * Stand-in for a real feature component reading the theme + query-client
 * context this tree provides. Not "renders without crashing": each field
 * is asserted against the exact value the corresponding provider should
 * have produced.
 */
function ContextProbe() {
  const theme = useTheme();
  const queryClient = useQueryClient();
  return (
    <div
      data-testid="probe"
      data-primary-main={theme.vars.palette.primary.main}
      data-has-query-client={String(queryClient !== undefined)}
    />
  );
}

/**
 * Exact fixture shape unit S8 used for its own I18nProvider proof
 * (`src/shared/i18n/I18nProvider.test.tsx`'s `Greeting`) — the published
 * `TFunction = (key, fallback, options?) => string` contract, so this test
 * exercises AppProviders' real I18nProvider the same way a Wave-2
 * `shared/ui`/`features/*` component will.
 */
function Greeting({ translate }: { translate: TFunction }) {
  return <p>{translate('demo.appProviders.greeting', 'Hello (fallback, should not render)')}</p>;
}

function ThrowingChild(): never {
  throw new Error('boom from AppProviders integration test');
}

describe('AppProviders', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
    i18n.removeResourceBundle('en', 'translation');
    // The channel-C global is document-scoped state; leaving it set would
    // silently re-theme every later test in this file.
    delete (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL];
  });

  it('provides theme + query client context to descendants (not just "renders without crashing")', () => {
    const { getByTestId } = render(
      <AppProviders>
        <ContextProbe />
      </AppProviders>,
    );

    const probe = getByTestId('probe');
    // Post-mount, theme.vars.* resolves to the bare var() form (see
    // BrandThemeProvider.test.tsx for why); the real colour value lives in
    // the injected stylesheet, asserted below through the FULL composed
    // AppProviders tree (not BrandThemeProvider in isolation).
    expect(probe.dataset['primaryMain']).toBe('var(--el-palette-primary-main)');
    expect(probe.dataset['hasQueryClient']).toBe('true');

    const styleText = [...document.querySelectorAll('style')]
      .map((style) => style.textContent ?? '')
      .join('\n')
      .toLowerCase();
    expect(styleText).toContain(
      `--el-palette-primary-main:${String(DEFAULT_BRAND_PACK.schemes.dark['primary.main']).toLowerCase()}`,
    );
  });

  it('RED/GREEN (c): I18nProvider resolves a real en.json key through the whole provider tree', () => {
    i18n.addResourceBundle('en', 'translation', { 'demo.appProviders.greeting': 'Hello from en.json' }, true, true);

    render(
      <AppProviders>
        <Greeting translate={t} />
      </AppProviders>,
    );

    expect(screen.getByText('Hello from en.json')).toBeTruthy();
  });

  it('RED case paired with the above: the same tree falls back gracefully when the key is missing', () => {
    render(
      <AppProviders>
        <Greeting translate={t} />
      </AppProviders>,
    );

    expect(screen.getByText('Hello (fallback, should not render)')).toBeTruthy();
  });

  /**
   * Channel C reaches the theme (issue #136 C). This provider used to render
   * `<BrandThemeProvider>` with no `pack`, so the compiled
   * `DEFAULT_BRAND_PACK` always won regardless of what elitea-main served —
   * "a tenant brand pack changes the app with no rebuild" (JRNY-030) was
   * simply not true. The served hue must reach a real, emitted CSS variable,
   * not merely be readable from the global.
   */
  it('builds the theme from the SERVED brand pack, not the compiled default', () => {
    // Computed, not a literal (R-T1 bans raw colour literals in `src/`).
    const servedHue = formatHex(hslaToRgba({ h: 330, s: 1, l: 0.5, a: 1 }));
    (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL] = {
      ...DEFAULT_BRAND_PACK,
      id: 'autotest-served',
      product: { name: 'Contoso Cloud', shortName: 'Contoso' },
      brand: { hue: servedHue },
      // Empty records so `brand.hue` drives every token instead of being
      // shadowed by the default pack's 362 stated values.
      schemes: { light: {}, dark: {} },
    };

    render(
      <AppProviders>
        <ContextProbe />
      </AppProviders>,
    );

    const emitted = [...document.querySelectorAll('style')]
      .map((element) => element.textContent ?? '')
      .join('\n')
      .toLowerCase();
    // The compiled default's dark primary must be GONE...
    expect(emitted).not.toContain(
      `--el-palette-primary-main:${String(DEFAULT_BRAND_PACK.schemes.dark['primary.main']).toLowerCase()}`,
    );
    // ...and a primary must still be published (a pack that failed to parse
    // would silently reinstate the line asserted absent above, so this pair
    // cannot both pass by accident).
    expect(emitted).toContain('--el-palette-primary-main:');
    expect(resolveBrandPack().brand.hue).toBe(servedHue);
  });

  it('the composed AppErrorBoundary catches a child render error without crashing the tree', () => {
    render(
      <AppProviders>
        <ThrowingChild />
      </AppProviders>,
    );

    expect(screen.getByRole('heading', { name: 'Something went wrong' })).toBeTruthy();
  });
});

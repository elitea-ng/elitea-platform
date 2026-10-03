/**
 * One scheme of the preview (ADR-0024 WP9): the sample shell under the
 * scoped theme, the derived-palette swatch strip and the WCAG AA check on
 * text-over-primary. The three read the SAME built theme, so they agree
 * with each other and with what the app would paint.
 *
 * The theme is built under `PREVIEW_THEME_SCOPE` for the reason
 * `pages/admin/BrandingPreview.tsx` gives: the page's own chrome is a MUI
 * theme too, and two themes sharing a prefix or a scheme selector paint
 * each other. `colorSchemeNode={null}` keeps the provider from stamping the
 * scheme on `<html>`; each surface selects its scheme with the attribute.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import { ThemeProvider } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import {
  PREVIEW_SCHEME_ATTRIBUTE,
  primaryContrastOf,
  swatchesOf,
  type BrandPack,
  type EliteaTheme,
  type PrimaryContrast,
  type SchemeName,
  type Swatch,
} from '@/shared/brand';
import { t } from '@/shared/i18n';

import type { LoadedAssets } from '../lib/assets';

import { SampleSurface } from './SampleSurface';

function schemeLabel(scheme: SchemeName): string {
  return scheme === 'light' ? t('entries.brandPreview.scheme.light', 'Light') : t('entries.brandPreview.scheme.dark', 'Dark');
}

function swatchLabel(id: Swatch['id']): string {
  switch (id) {
    case 'primary':
      return t('entries.brandPreview.swatch.primary', 'Primary');
    case 'onPrimary':
      return t('entries.brandPreview.swatch.onPrimary', 'Text on primary');
    case 'secondary':
      return t('entries.brandPreview.swatch.secondary', 'Secondary');
    case 'background':
      return t('entries.brandPreview.swatch.background', 'Background');
    case 'text':
      return t('entries.brandPreview.swatch.text', 'Text');
    case 'error':
      return t('entries.brandPreview.swatch.error', 'Error');
    case 'success':
      return t('entries.brandPreview.swatch.success', 'Success');
    case 'warning':
      return t('entries.brandPreview.swatch.warning', 'Warning');
    case 'info':
      return t('entries.brandPreview.swatch.info', 'Info');
  }
}

function SwatchStrip({ swatches, scheme }: { readonly swatches: readonly Swatch[]; readonly scheme: SchemeName }) {
  return (
    <Box
      component="ul"
      aria-label={`${schemeLabel(scheme)} ${t('entries.brandPreview.swatch.strip', 'derived palette')}`}
      data-testid={`brand-preview-swatches-${scheme}`}
      sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, listStyle: 'none', margin: 0, padding: 0 }}
    >
      {swatches.map((swatch) => (
        <Box
          component="li"
          key={swatch.id}
          title={`${swatchLabel(swatch.id)}: ${swatch.value}`}
          data-value={swatch.value}
          sx={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 0.5, width: '3.5rem' }}
        >
          <Box
            sx={(theme) => ({
              width: '2rem',
              height: '2rem',
              borderRadius: theme.vars.shape.radiusSm,
              border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
            })}
            // A computed value, never a literal: the swatch IS the derivation's answer.
            style={{ backgroundColor: swatch.value }}
          />
          <Typography variant="labelSmall" color="text.secondary" sx={{ textAlign: 'center' }}>
            {swatchLabel(swatch.id)}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function ContrastNotice({ contrast }: { readonly contrast: PrimaryContrast }) {
  const ratio = contrast.ratio === undefined ? '—' : `${contrast.ratio.toFixed(2)}:1`;
  if (contrast.meetsAA) {
    return (
      <Typography variant="bodySmall" color="text.secondary" data-testid={`brand-preview-contrast-${contrast.scheme}`}>
        {`${t('entries.brandPreview.contrast.ok', 'Text on primary meets WCAG AA')} (${ratio})`}
      </Typography>
    );
  }
  return (
    <Alert severity="warning" data-testid={`brand-preview-contrast-${contrast.scheme}`}>
      {`${t('entries.brandPreview.contrast.warning', 'Text on the primary colour falls below WCAG AA (4.5:1 for normal text)')} — ${ratio}. ${t(
        'entries.brandPreview.contrast.hint',
        'Set “onBrand” in the pack, or pick a darker or lighter hue.',
      )}`}
    </Alert>
  );
}

export interface SchemePreviewProps {
  readonly pack: BrandPack;
  readonly theme: EliteaTheme;
  readonly scheme: SchemeName;
  readonly assets: LoadedAssets;
}

export function SchemePreview({ pack, theme, scheme, assets }: SchemePreviewProps) {
  const swatches = swatchesOf(theme, scheme);
  const contrast = primaryContrastOf(theme, scheme);
  return (
    <Box component="section" aria-label={schemeLabel(scheme)} sx={{ display: 'flex', flexDirection: 'column', gap: 1, minWidth: 0 }}>
      <Typography variant="labelMedium" component="h3">
        {schemeLabel(scheme)}
      </Typography>
      <ThemeProvider theme={theme} colorSchemeNode={null}>
        <Box {...{ [PREVIEW_SCHEME_ATTRIBUTE]: scheme }} data-testid={`brand-preview-${scheme}`}>
          <SampleSurface pack={pack} assets={assets} />
        </Box>
      </ThemeProvider>
      <SwatchStrip swatches={swatches} scheme={scheme} />
      <ContrastNotice contrast={contrast} />
    </Box>
  );
}

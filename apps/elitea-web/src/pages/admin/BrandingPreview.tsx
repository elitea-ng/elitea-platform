/**
 * Admin › Branding — the live preview (ADR-0024 WP4; spec §4.3 E).
 *
 * A sample surface rendered TWICE, light and dark, inside a `ThemeProvider`
 * whose theme is `buildEliteaTheme(draftPack, PREVIEW_THEME_SCOPE)`: the real
 * builder over the pack the bootstrap route would serve after a save. The
 * swatch strip and the WCAG check beside it are read off that same theme, so
 * all three agree with each other and with what users will get.
 *
 * ## Why the theme is scoped
 *
 * MUI's `ThemeProvider` nested under another CSS-variable theme skips its
 * stylesheet when the two share a prefix, and re-declares every variable on
 * the document's scheme element when they share a selector — either the
 * preview shows the CONSOLE's brand or the console shows the preview's.
 * `PREVIEW_THEME_SCOPE` gives the preview its own prefix, its own scheme
 * attribute and a root selector nothing carries, so its variables reach only
 * the two surfaces below, each of which selects its scheme with the
 * attribute. `colorSchemeNode` is `null` so the provider does not stamp that
 * attribute on `<html>`.
 */
import { useMemo } from 'react';

import Alert from '@mui/material/Alert';
import AppBar from '@mui/material/AppBar';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Paper from '@mui/material/Paper';
import { ThemeProvider } from '@mui/material/styles';
import Toolbar from '@mui/material/Toolbar';
import Typography from '@mui/material/Typography';

import {
  PREVIEW_ROOT_CLASS,
  PREVIEW_SCHEME_ATTRIBUTE,
  PREVIEW_THEME_SCOPE,
  buildEliteaTheme,
  primaryContrastOf,
  swatchesOf,
  type BrandPack,
  type EliteaTheme,
  type PrimaryContrast,
  type SchemeName,
  type Swatch,
} from '@/shared/brand';
import { t } from '@/shared/i18n';

const SCHEMES: readonly SchemeName[] = ['light', 'dark'];

function schemeLabel(scheme: SchemeName): string {
  return scheme === 'light'
    ? t('pages.admin.branding.preview.light', 'Light')
    : t('pages.admin.branding.preview.dark', 'Dark');
}

function swatchLabel(id: Swatch['id']): string {
  switch (id) {
    case 'primary':
      return t('pages.admin.branding.swatch.primary', 'Primary');
    case 'onPrimary':
      return t('pages.admin.branding.swatch.onPrimary', 'Text on primary');
    case 'secondary':
      return t('pages.admin.branding.swatch.secondary', 'Secondary');
    case 'background':
      return t('pages.admin.branding.swatch.background', 'Background');
    case 'text':
      return t('pages.admin.branding.swatch.text', 'Text');
    case 'error':
      return t('pages.admin.branding.swatch.error', 'Error');
    case 'success':
      return t('pages.admin.branding.swatch.success', 'Success');
    case 'warning':
      return t('pages.admin.branding.swatch.warning', 'Warning');
    case 'info':
      return t('pages.admin.branding.swatch.info', 'Info');
  }
}

function SwatchStrip({ swatches, scheme }: { readonly swatches: readonly Swatch[]; readonly scheme: SchemeName }) {
  return (
    <Box
      component="ul"
      aria-label={`${schemeLabel(scheme)} ${t('pages.admin.branding.swatch.strip', 'derived palette')}`}
      data-testid={`branding-swatches-${scheme}`}
      sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.5rem', listStyle: 'none', margin: 0, padding: 0 }}
    >
      {swatches.map((swatch) => (
        <Box
          component="li"
          key={swatch.id}
          title={`${swatchLabel(swatch.id)}: ${swatch.value}`}
          data-testid={`branding-swatch-${scheme}-${swatch.id}`}
          data-value={swatch.value}
          sx={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '0.25rem', width: '3.5rem' }}
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
      <Typography variant="bodySmall" color="text.secondary" data-testid={`branding-contrast-${contrast.scheme}`}>
        {`${t('pages.admin.branding.contrast.ok', 'Text on primary meets WCAG AA')} (${ratio})`}
      </Typography>
    );
  }
  return (
    <Alert severity="warning" data-testid={`branding-contrast-${contrast.scheme}`}>
      {`${t(
        'pages.admin.branding.contrast.warning',
        'Text on the primary colour falls below WCAG AA (4.5:1 for normal text)',
      )} — ${ratio}. ${t('pages.admin.branding.contrast.hint', 'Set “Text on brand colour” or pick a darker or lighter brand colour. Saving is not blocked.')}`}
    </Alert>
  );
}

function SampleSurface({ pack, scheme }: { readonly pack: BrandPack; readonly scheme: SchemeName }) {
  return (
    <Box
      {...{ [PREVIEW_SCHEME_ATTRIBUTE]: scheme }}
      data-testid={`branding-preview-${scheme}`}
      sx={(theme) => ({
        borderRadius: theme.vars.shape.radiusMd,
        overflow: 'hidden',
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        backgroundColor: theme.vars.palette.background.default,
        color: theme.vars.palette.text.primary,
        fontFamily: theme.typography.fontFamily,
      })}
    >
      <AppBar position="static" elevation={0}>
        <Toolbar variant="dense" sx={{ gap: '0.75rem' }}>
          {pack.assets.logoMark.startsWith('/') ? (
            <Box component="img" src={pack.assets.logoMark} alt="" sx={{ height: '1.5rem' }} />
          ) : null}
          <Typography variant="headingSmall" component="span">
            {pack.product.name}
          </Typography>
        </Toolbar>
      </AppBar>
      <Box sx={{ padding: '1rem', display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
        <Typography variant="headingMedium" component="p">
          {pack.product.tagline ?? t('pages.admin.branding.preview.heading', 'A heading in the brand type')}
        </Typography>
        <Paper sx={{ padding: '0.75rem', display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <Typography variant="bodyMedium">
            {t(
              'pages.admin.branding.preview.body',
              'Body text on a card. Links, chips and buttons take the derived accent.',
            )}
          </Typography>
          <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', alignItems: 'center' }}>
            <Button variant="elitea" color="primary" size="small">
              {t('pages.admin.branding.preview.primaryAction', 'Primary action')}
            </Button>
            <Button variant="secondary" size="small">
              {t('pages.admin.branding.preview.secondaryAction', 'Secondary')}
            </Button>
            <Chip size="small" label={pack.product.shortName} />
          </Box>
        </Paper>
      </Box>
    </Box>
  );
}

interface SchemePreviewProps {
  readonly pack: BrandPack;
  readonly theme: EliteaTheme;
  readonly scheme: SchemeName;
}

function SchemePreview({ pack, theme, scheme }: SchemePreviewProps) {
  const swatches = useMemo(() => swatchesOf(theme, scheme), [theme, scheme]);
  const contrast = useMemo(() => primaryContrastOf(theme, scheme), [theme, scheme]);
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
      <Typography variant="labelMedium" component="h3">
        {schemeLabel(scheme)}
      </Typography>
      <ThemeProvider theme={theme} colorSchemeNode={null}>
        <SampleSurface pack={pack} scheme={scheme} />
      </ThemeProvider>
      <SwatchStrip swatches={swatches} scheme={scheme} />
      <ContrastNotice contrast={contrast} />
    </Box>
  );
}

export interface BrandingPreviewProps {
  /** The pack the bootstrap route would serve after a save. */
  readonly pack: BrandPack;
}

export function BrandingPreview({ pack }: BrandingPreviewProps) {
  const theme = useMemo(() => buildEliteaTheme(pack, PREVIEW_THEME_SCOPE), [pack]);
  // The class IS the scope's `rootSelector`: MUI emits the spacing unit and
  // the `--elp-shape-*` radii under that selector only, so both surfaces need
  // it on a common ancestor or every radius inside the preview resolves to
  // nothing. See `PREVIEW_ROOT_CLASS` in `shared/brand/preview.ts`.
  return (
    <Box
      component="section"
      className={PREVIEW_ROOT_CLASS}
      aria-labelledby="branding-preview-heading"
      data-testid="branding-preview"
      sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}
    >
      <Typography id="branding-preview-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.preview.title', 'Preview')}
      </Typography>
      {SCHEMES.map((scheme) => (
        <SchemePreview key={scheme} pack={pack} theme={theme} scheme={scheme} />
      ))}
    </Box>
  );
}

/**
 * Admin › Branding — colour, typography, shape and density (ADR-0024 WP4).
 *
 * The brand colour is the one input the whole derivation hangs from
 * (`shared/brand/color.ts`): every accent, surface tint and neutral is
 * computed from it. Two controls edit the same value — a native colour picker
 * and the hex text — because a picker cannot express "inherit" and a text
 * field cannot show a colour.
 */
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { BrandPack } from '@/shared/brand';
import { formatHex, parseColor } from '@/shared/brand/color';
import { t } from '@/shared/i18n';

import { BrandingNumberField, BrandingTextField, inheritHelperText } from './BrandingField';
import type { BrandingFieldGroupProps } from './BrandingIdentityFields';
import {
  brandingFieldSource,
  isSixDigitHex,
  type BrandingNumberKey,
  type BrandingTextKey,
} from './brandingValues';

/** A native colour input takes `#rrggbb` only; anything else is shown as the base. */
function pickerValue(value: string, fallback: string): string {
  if (isSixDigitHex(value)) return value.trim().toLowerCase();
  const parsed = parseColor(fallback);
  return parsed === null ? `#${'000000'}` : formatHex({ ...parsed, a: 1 });
}

interface ColourFieldProps {
  readonly fieldKey: BrandingTextKey;
  readonly label: string;
  readonly description: string;
  readonly value: string;
  readonly inherited: string;
  readonly source: ReturnType<typeof brandingFieldSource>;
  readonly error: string | undefined;
  readonly disabled: boolean;
  readonly onChange: (value: string) => void;
}

function ColourField({
  fieldKey,
  label,
  description,
  value,
  inherited,
  source,
  error,
  disabled,
  onChange,
}: ColourFieldProps) {
  const notHex = value.trim() !== '' && !isSixDigitHex(value);
  const hint = notHex
    ? t('pages.admin.branding.field.hexHint', 'Six hex digits after the # sign.')
    : undefined;
  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', alignItems: 'flex-start' }}>
      <Box
        component="input"
        type="color"
        id={`branding-${fieldKey}-picker`}
        aria-label={`${label} ${t('pages.admin.branding.field.picker', 'picker')}`}
        data-testid={`branding-picker-${fieldKey}`}
        value={pickerValue(value, inherited)}
        disabled={disabled}
        onChange={(event: React.ChangeEvent<HTMLInputElement>) => onChange(event.target.value)}
        sx={(theme) => ({
          width: '2.5rem',
          height: '2.5rem',
          padding: 0,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          borderRadius: theme.vars.shape.radiusSm,
          background: 'transparent',
          cursor: disabled ? 'default' : 'pointer',
          flex: '0 0 auto',
          marginTop: '0.125rem',
        })}
      />
      <TextField
        id={`branding-${fieldKey}`}
        size="small"
        label={label}
        value={value}
        disabled={disabled}
        error={error !== undefined}
        helperText={error ?? hint ?? inheritHelperText(source, inherited, description)}
        onChange={(event) => onChange(event.target.value)}
        slotProps={{ htmlInput: { 'data-testid': `branding-field-${fieldKey}`, spellCheck: false } }}
        fullWidth
      />
    </Box>
  );
}

interface NumberSpec {
  readonly key: BrandingNumberKey;
  readonly label: string;
  readonly description: string;
  readonly step: number;
  readonly inherited: (pack: BrandPack) => number;
}

function numberSpecs(): readonly NumberSpec[] {
  const px = t('pages.admin.branding.field.pixels', 'Pixels.');
  return [
    {
      key: 'base_size',
      label: t('pages.admin.branding.field.baseSize', 'Base font size'),
      description: t('pages.admin.branding.field.baseSize.description', 'Pixels, 12 to 18.'),
      step: 1,
      inherited: (pack) => pack.typography.baseSize,
    },
    {
      key: 'scale',
      label: t('pages.admin.branding.field.scale', 'Type scale'),
      description: t(
        'pages.admin.branding.field.scale.description',
        'Ratio between heading steps, 1.05 to 1.5.',
      ),
      step: 0.05,
      inherited: (pack) => pack.typography.scale,
    },
    { key: 'radius_sm', label: t('pages.admin.branding.field.radiusSm', 'Small radius'), description: px, step: 1, inherited: (pack) => pack.shape.radiusSm },
    { key: 'radius_md', label: t('pages.admin.branding.field.radiusMd', 'Medium radius'), description: px, step: 1, inherited: (pack) => pack.shape.radiusMd },
    { key: 'radius_lg', label: t('pages.admin.branding.field.radiusLg', 'Large radius'), description: px, step: 1, inherited: (pack) => pack.shape.radiusLg },
    {
      key: 'radius_pill',
      label: t('pages.admin.branding.field.radiusPill', 'Pill radius'),
      description: t('pages.admin.branding.field.radiusPill.description', 'Pixels; 9999 for a full pill.'),
      step: 1,
      inherited: (pack) => pack.shape.radiusPill,
    },
  ];
}

export function BrandingStyleFields({
  values,
  basePack,
  layers,
  disabled,
  fieldError,
  onChange,
}: BrandingFieldGroupProps) {
  const errorFor = (key: string): string | undefined =>
    fieldError?.key === key ? fieldError.message : undefined;
  const densitySource = brandingFieldSource(values, 'density', layers);
  return (
    <Box component="section" aria-labelledby="branding-style-heading" sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography id="branding-style-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.group.style', 'Colour, type and shape')}
      </Typography>

      <ColourField
        fieldKey="brand_hue"
        label={t('pages.admin.branding.field.brandHue', 'Brand colour')}
        description={t(
          'pages.admin.branding.field.brandHue.description',
          'Every accent and surface tint is derived from it.',
        )}
        value={values.brand_hue}
        inherited={basePack.brand.hue}
        source={brandingFieldSource(values, 'brand_hue', layers)}
        error={errorFor('brand_hue')}
        disabled={disabled}
        onChange={(value) => onChange('brand_hue', value)}
      />
      <ColourField
        fieldKey="brand_on_brand"
        label={t('pages.admin.branding.field.brandOnBrand', 'Text on brand colour')}
        description={t(
          'pages.admin.branding.field.brandOnBrand.description',
          'Leave empty to let the theme pick black or white by contrast.',
        )}
        value={values.brand_on_brand}
        inherited={basePack.brand.onBrand ?? ''}
        source={brandingFieldSource(values, 'brand_on_brand', layers)}
        error={errorFor('brand_on_brand')}
        disabled={disabled}
        onChange={(value) => onChange('brand_on_brand', value)}
      />

      <BrandingTextField
        fieldKey="font_family"
        label={t('pages.admin.branding.field.fontFamily', 'Font family')}
        description={t(
          'pages.admin.branding.field.fontFamily.description',
          'CSS font-family list for text. Self-hosted faces are declared below.',
        )}
        value={values.font_family}
        inherited={basePack.typography.fontFamily}
        source={brandingFieldSource(values, 'font_family', layers)}
        error={errorFor('font_family')}
        disabled={disabled}
        onChange={(value) => onChange('font_family', value)}
      />
      <BrandingTextField
        fieldKey="font_family_mono"
        label={t('pages.admin.branding.field.fontFamilyMono', 'Monospace font family')}
        description={t('pages.admin.branding.field.fontFamilyMono.description', 'CSS font-family list for code.')}
        value={values.font_family_mono}
        inherited={basePack.typography.fontFamilyMono}
        source={brandingFieldSource(values, 'font_family_mono', layers)}
        error={errorFor('font_family_mono')}
        disabled={disabled}
        onChange={(value) => onChange('font_family_mono', value)}
      />

      <Box sx={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(11rem, 1fr))', gap: '1rem' }}>
        {numberSpecs().map((spec) => (
          <BrandingNumberField
            key={spec.key}
            fieldKey={spec.key}
            label={spec.label}
            description={spec.description}
            value={values[spec.key]}
            inherited={String(spec.inherited(basePack))}
            source={brandingFieldSource(values, spec.key, layers)}
            error={errorFor(spec.key)}
            disabled={disabled}
            step={spec.step}
            onChange={(value) => onChange(spec.key, value)}
          />
        ))}
      </Box>

      <TextField
        id="branding-density"
        select
        size="small"
        label={t('pages.admin.branding.field.density', 'Density')}
        value={values.density === 'compact' || values.density === 'comfortable' ? values.density : ''}
        disabled={disabled}
        error={errorFor('density') !== undefined}
        helperText={errorFor('density') ?? inheritHelperText(densitySource, basePack.shape.density)}
        onChange={(event) => onChange('density', event.target.value)}
        slotProps={{ htmlInput: { 'data-testid': 'branding-field-density' } }}
        fullWidth
      >
        <MenuItem value="">{t('pages.admin.branding.density.inherit', 'Inherit')}</MenuItem>
        <MenuItem value="comfortable">{t('pages.admin.branding.density.comfortable', 'Comfortable')}</MenuItem>
        <MenuItem value="compact">{t('pages.admin.branding.density.compact', 'Compact')}</MenuItem>
      </TextField>
    </Box>
  );
}

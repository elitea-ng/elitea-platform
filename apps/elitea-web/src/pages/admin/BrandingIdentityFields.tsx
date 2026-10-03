/**
 * Admin › Branding — the product identity group: name, short name, tagline
 * and the two links (ADR-0024 WP4).
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { BrandPack } from '@/shared/brand';
import { t } from '@/shared/i18n';

import { BrandingTextField } from './BrandingField';
import {
  brandingFieldSource,
  type BrandingLayers,
  type BrandingTextKey,
  type BrandingValues,
} from './brandingValues';
import type { BrandingFieldError, BrandingScalarKey } from './useAdminBrandingPage';

export interface BrandingFieldGroupProps {
  readonly values: BrandingValues;
  readonly basePack: BrandPack;
  readonly layers: BrandingLayers;
  readonly disabled: boolean;
  readonly fieldError: BrandingFieldError | undefined;
  readonly onChange: <K extends BrandingScalarKey>(key: K, value: BrandingValues[K]) => void;
}

interface IdentityField {
  readonly key: BrandingTextKey;
  readonly label: string;
  readonly description: string;
  readonly inherited: (pack: BrandPack) => string;
}

function identityFields(): readonly IdentityField[] {
  return [
    {
      key: 'product_name',
      label: t('pages.admin.branding.field.productName', 'Product name'),
      description: t(
        'pages.admin.branding.field.productName.description',
        'Shown in the document title, the login page and e-mail.',
      ),
      inherited: (pack) => pack.product.name,
    },
    {
      key: 'product_short_name',
      label: t('pages.admin.branding.field.productShortName', 'Short name'),
      description: t(
        'pages.admin.branding.field.productShortName.description',
        'Used where the full name does not fit.',
      ),
      inherited: (pack) => pack.product.shortName,
    },
    {
      key: 'product_tagline',
      label: t('pages.admin.branding.field.productTagline', 'Tagline'),
      description: t(
        'pages.admin.branding.field.productTagline.description',
        'A short line under the name on the login page.',
      ),
      inherited: (pack) => pack.product.tagline ?? '',
    },
    {
      key: 'docs_url',
      label: t('pages.admin.branding.field.docsUrl', 'Documentation URL'),
      description: t(
        'pages.admin.branding.field.docsUrl.description',
        'Absolute http(s) URL the help links open.',
      ),
      inherited: (pack) => pack.product.docsUrl ?? '',
    },
    {
      key: 'support_url',
      label: t('pages.admin.branding.field.supportUrl', 'Support URL'),
      description: t(
        'pages.admin.branding.field.supportUrl.description',
        'Absolute http(s) URL the support links open.',
      ),
      inherited: (pack) => pack.product.supportUrl ?? '',
    },
  ];
}

export function BrandingIdentityFields({
  values,
  basePack,
  layers,
  disabled,
  fieldError,
  onChange,
}: BrandingFieldGroupProps) {
  return (
    <Box component="section" aria-labelledby="branding-identity-heading" sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography id="branding-identity-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.group.identity', 'Product identity')}
      </Typography>
      {identityFields().map((field) => (
        <BrandingTextField
          key={field.key}
          fieldKey={field.key}
          label={field.label}
          description={field.description}
          value={values[field.key]}
          inherited={field.inherited(basePack)}
          source={brandingFieldSource(values, field.key, layers)}
          error={fieldError?.key === field.key ? fieldError.message : undefined}
          disabled={disabled}
          onChange={(value) => onChange(field.key, value)}
        />
      ))}
    </Box>
  );
}

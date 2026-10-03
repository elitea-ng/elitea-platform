/**
 * Admin › Branding — the asset uploaders (ADR-0024 WP4).
 *
 * Each control uploads ONE file to `POST /admin/branding/assets/{kind}` and
 * writes the path the server answered into the draft; nothing is stored
 * until the operator saves. The server sniffs the bytes, caps the size per
 * kind and refuses an SVG that could script or fetch, and its reason is shown
 * verbatim under the control that sent it.
 *
 * The e-mail logo (`logo_email`, ADR-0024 WP7) is a raster only — mail
 * clients render neither SVG nor WebP reliably, so the route accepts PNG and
 * WebP and the control offers the same. Its inherited value is read off the
 * served pack's raw document (`effectiveLogoEmail`), since the UI's pack
 * schema carries no field for it.
 */
import { useRef, type ReactNode } from 'react';

import UploadFileOutlinedIcon from '@mui/icons-material/UploadFileOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormHelperText from '@mui/material/FormHelperText';
import Typography from '@mui/material/Typography';

import type { BrandPack } from '@/shared/brand';
import { t } from '@/shared/i18n';

import type { BrandingAssetKind } from './api/adminBrandingApi';
import { inheritHelperText } from './BrandingField';
import type { BrandingFieldGroupProps } from './BrandingIdentityFields';
import {
  brandingFieldSource,
  type BrandingAssetKey,
  type BrandingFieldSource,
} from './brandingValues';
import type { BrandingUploadError, BrandingUploadTarget } from './useAdminBrandingPage';

const IMAGE_ACCEPT = '.svg,.png,.jpg,.jpeg,.webp,image/svg+xml,image/png,image/jpeg,image/webp';
const FAVICON_ACCEPT = '.svg,.png,.ico,image/svg+xml,image/png,image/x-icon';
const EMAIL_LOGO_ACCEPT = '.png,.webp,image/png,image/webp';

interface AssetSpec {
  readonly kind: BrandingAssetKind;
  readonly fieldKey: BrandingAssetKey;
  readonly label: string;
  readonly accept: string;
  readonly inherited: (pack: BrandPack, logoEmail: string) => string;
}

function assetSpecs(): readonly AssetSpec[] {
  return [
    {
      kind: 'logo-full',
      fieldKey: 'logo_full',
      label: t('pages.admin.branding.asset.logoFull', 'Logo'),
      accept: IMAGE_ACCEPT,
      inherited: (pack) => pack.assets.logoFull,
    },
    {
      kind: 'logo-mark',
      fieldKey: 'logo_mark',
      label: t('pages.admin.branding.asset.logoMark', 'Logo mark'),
      accept: IMAGE_ACCEPT,
      inherited: (pack) => pack.assets.logoMark,
    },
    {
      kind: 'favicon',
      fieldKey: 'favicon',
      label: t('pages.admin.branding.asset.favicon', 'Favicon'),
      accept: FAVICON_ACCEPT,
      inherited: (pack) => pack.assets.favicon,
    },
    {
      kind: 'login-art',
      fieldKey: 'login_art',
      label: t('pages.admin.branding.asset.loginArt', 'Login artwork'),
      accept: IMAGE_ACCEPT,
      inherited: (pack) => pack.assets.loginArt ?? '',
    },
    {
      kind: 'logo-email',
      fieldKey: 'logo_email',
      label: t('pages.admin.branding.asset.logoEmail', 'E-mail logo'),
      accept: EMAIL_LOGO_ACCEPT,
      inherited: (_pack, logoEmail) => logoEmail,
    },
  ];
}

/** Only a same-origin path can be shown; the compiled default's `./brand/…` is relative to the app, not to this console. */
function previewSrc(path: string): string | undefined {
  return path.startsWith('/') ? path : undefined;
}

interface AssetUploaderProps {
  readonly spec: AssetSpec;
  readonly value: string;
  readonly inherited: string;
  readonly source: BrandingFieldSource;
  readonly busy: boolean;
  readonly error: string | undefined;
  readonly disabled: boolean;
  readonly onFile: (file: File) => void;
  readonly onClear: () => void;
}

function AssetUploader({
  spec,
  value,
  inherited,
  source,
  busy,
  error,
  disabled,
  onFile,
  onClear,
}: AssetUploaderProps): ReactNode {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const shown = value !== '' ? value : inherited;
  const src = previewSrc(shown);
  const helper = error ?? inheritHelperText(source, inherited);

  return (
    <Box
      data-testid={`branding-asset-${spec.kind}`}
      sx={(theme) => ({
        display: 'flex',
        gap: '1rem',
        alignItems: 'flex-start',
        padding: '0.75rem',
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <Box
        sx={(theme) => ({
          width: '4rem',
          height: '4rem',
          flex: '0 0 auto',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          overflow: 'hidden',
          borderRadius: theme.vars.shape.radiusSm,
          backgroundColor: theme.vars.palette.background.default,
        })}
      >
        {src === undefined ? (
          <Typography variant="labelSmall" color="text.secondary">
            {t('pages.admin.branding.asset.noPreview', 'No preview')}
          </Typography>
        ) : (
          <Box component="img" src={src} alt={spec.label} sx={{ maxWidth: '100%', maxHeight: '100%' }} />
        )}
      </Box>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem', minWidth: 0, flex: 1 }}>
        <Typography variant="labelMedium" component="span">
          {spec.label}
        </Typography>
        <Typography
          variant="bodySmall"
          color="text.secondary"
          data-testid={`branding-asset-path-${spec.kind}`}
          sx={{ overflowWrap: 'anywhere' }}
        >
          {shown === '' ? t('pages.admin.branding.asset.none', 'Not set') : shown}
        </Typography>
        <FormHelperText error={error !== undefined}>{helper}</FormHelperText>
        <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
          <input
            ref={inputRef}
            type="file"
            hidden
            accept={spec.accept}
            data-testid={`branding-upload-input-${spec.kind}`}
            aria-label={`${spec.label} ${t('pages.admin.branding.asset.file', 'file')}`}
            onChange={(event) => {
              const file = event.target.files?.[0];
              event.target.value = '';
              if (file !== undefined) onFile(file);
            }}
          />
          <Button
            size="small"
            variant="secondary"
            startIcon={<UploadFileOutlinedIcon />}
            disabled={disabled || busy}
            onClick={() => inputRef.current?.click()}
            data-testid={`branding-upload-${spec.kind}`}
          >
            {busy
              ? t('pages.admin.branding.asset.uploading', 'Uploading…')
              : t('pages.admin.branding.asset.upload', 'Upload')}
          </Button>
          {value === '' ? null : (
            <Button size="small" variant="tertiary" disabled={disabled || busy} onClick={onClear}>
              {t('pages.admin.branding.asset.clear', 'Clear (inherit)')}
            </Button>
          )}
        </Box>
      </Box>
    </Box>
  );
}

export interface BrandingAssetFieldsProps extends BrandingFieldGroupProps {
  /** The served pack's `assets.logoEmail`, or empty — see `effectiveLogoEmail`. */
  readonly inheritedLogoEmail: string;
  readonly uploadingKind: BrandingAssetKind | undefined;
  readonly uploadError: BrandingUploadError | undefined;
  readonly onUpload: (kind: BrandingAssetKind, file: File, target: BrandingUploadTarget) => void;
}

export function BrandingAssetFields({
  values,
  basePack,
  layers,
  disabled,
  fieldError,
  onChange,
  inheritedLogoEmail,
  uploadingKind,
  uploadError,
  onUpload,
}: BrandingAssetFieldsProps) {
  return (
    <Box component="section" aria-labelledby="branding-assets-heading" sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography id="branding-assets-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.group.assets', 'Logos and images')}
      </Typography>
      {assetSpecs().map((spec) => {
        const key = spec.fieldKey;
        const error =
          uploadError?.kind === spec.kind
            ? uploadError.message
            : fieldError?.key === key
              ? fieldError.message
              : undefined;
        return (
          <AssetUploader
            key={spec.kind}
            spec={spec}
            value={values[key]}
            inherited={spec.inherited(basePack, inheritedLogoEmail)}
            source={brandingFieldSource(values, key, layers)}
            busy={uploadingKind === spec.kind}
            error={error}
            disabled={disabled}
            onFile={(file) => onUpload(spec.kind, file, { field: key })}
            onClear={() => onChange(key, '')}
          />
        );
      })}
    </Box>
  );
}

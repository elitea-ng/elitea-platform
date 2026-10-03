/**
 * Admin › Branding — the self-hosted font faces (ADR-0024 WP4).
 *
 * Up to two `@font-face` declarations the bootstrap route emits: a family
 * name, an uploaded WOFF2 and optionally a weight and a style. The `url` is
 * never typed — it is the path an upload to `assets/font` answered, which is
 * the only value the server accepts for it.
 */
import { useRef } from 'react';

import AddIcon from '@mui/icons-material/Add';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import UploadFileOutlinedIcon from '@mui/icons-material/UploadFileOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormHelperText from '@mui/material/FormHelperText';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { BrandingFontFace } from './brandingValues';
import type { BrandingFieldError, BrandingUploadError, BrandingUploadTarget } from './useAdminBrandingPage';

export const MAX_FONT_FACES = 2;

export interface BrandingFontFacesEditorProps {
  readonly faces: readonly BrandingFontFace[];
  /** The faces the served pack carries — what an empty list inherits. */
  readonly effectiveFaces: readonly BrandingFontFace[];
  readonly disabled: boolean;
  readonly uploading: boolean;
  readonly uploadError: BrandingUploadError | undefined;
  readonly fieldError: BrandingFieldError | undefined;
  readonly onChange: (faces: readonly BrandingFontFace[]) => void;
  readonly onUpload: (file: File, target: BrandingUploadTarget) => void;
}

interface FaceRowProps {
  readonly index: number;
  readonly face: BrandingFontFace;
  readonly disabled: boolean;
  readonly uploading: boolean;
  readonly onPatch: (patch: Partial<BrandingFontFace>) => void;
  readonly onRemove: () => void;
  readonly onFile: (file: File) => void;
}

function FaceRow({ index, face, disabled, uploading, onPatch, onRemove, onFile }: FaceRowProps) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const ordinal = String(index + 1);
  return (
    <Box
      data-testid={`branding-font-face-${index}`}
      sx={(theme) => ({
        display: 'grid',
        gridTemplateColumns: 'minmax(8rem, 2fr) minmax(5rem, 1fr) minmax(6rem, 1fr) auto',
        gap: '0.75rem',
        alignItems: 'flex-start',
        padding: '0.75rem',
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <TextField
        size="small"
        label={`${t('pages.admin.branding.font.family', 'Family')} ${ordinal}`}
        value={face.family}
        disabled={disabled}
        onChange={(event) => onPatch({ family: event.target.value })}
        slotProps={{ htmlInput: { 'data-testid': `branding-font-family-${index}` } }}
      />
      <TextField
        size="small"
        label={`${t('pages.admin.branding.font.weight', 'Weight')} ${ordinal}`}
        value={face.weight ?? ''}
        disabled={disabled}
        helperText={t('pages.admin.branding.font.weight.hint', 'e.g. 400 or 100 900')}
        onChange={(event) => onPatch({ weight: event.target.value })}
      />
      <TextField
        select
        size="small"
        label={`${t('pages.admin.branding.font.style', 'Style')} ${ordinal}`}
        value={face.style ?? ''}
        disabled={disabled}
        onChange={(event) => onPatch({ style: event.target.value })}
      >
        <MenuItem value="">{t('pages.admin.branding.font.style.unset', 'Unset')}</MenuItem>
        <MenuItem value="normal">{t('pages.admin.branding.font.style.normal', 'Normal')}</MenuItem>
        <MenuItem value="italic">{t('pages.admin.branding.font.style.italic', 'Italic')}</MenuItem>
      </TextField>
      <IconButton
        aria-label={`${t('pages.admin.branding.font.remove', 'Remove face')} ${ordinal}`}
        disabled={disabled}
        onClick={onRemove}
        size="small"
      >
        <DeleteOutlineIcon fontSize="small" />
      </IconButton>
      <Box sx={{ gridColumn: '1 / -1', display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
        <input
          ref={inputRef}
          type="file"
          hidden
          accept=".woff2,font/woff2"
          data-testid={`branding-upload-input-font-${index}`}
          aria-label={`${t('pages.admin.branding.font.file', 'WOFF2 file for face')} ${ordinal}`}
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
          disabled={disabled || uploading}
          onClick={() => inputRef.current?.click()}
          data-testid={`branding-upload-font-${index}`}
        >
          {uploading
            ? t('pages.admin.branding.asset.uploading', 'Uploading…')
            : t('pages.admin.branding.font.upload', 'Upload WOFF2')}
        </Button>
        <Typography
          variant="bodySmall"
          color="text.secondary"
          data-testid={`branding-font-url-${index}`}
          sx={{ overflowWrap: 'anywhere' }}
        >
          {face.url === '' ? t('pages.admin.branding.font.noFile', 'No file uploaded yet') : face.url}
        </Typography>
      </Box>
    </Box>
  );
}

export function BrandingFontFacesEditor({
  faces,
  effectiveFaces,
  disabled,
  uploading,
  uploadError,
  fieldError,
  onChange,
  onUpload,
}: BrandingFontFacesEditorProps) {
  const error =
    uploadError?.kind === 'font'
      ? uploadError.message
      : fieldError?.key === 'font_faces'
        ? fieldError.message
        : undefined;
  const inheritedSummary =
    effectiveFaces.length === 0
      ? t('pages.admin.branding.font.inheritsNone', 'The layer below declares no faces.')
      : `${t('pages.admin.branding.font.inherits', 'The layer below declares:')} ${effectiveFaces
          .map((face) => face.family)
          .join(', ')}`;

  const patch = (index: number, change: Partial<BrandingFontFace>): void => {
    onChange(faces.map((face, at) => (at === index ? { ...face, ...change } : face)));
  };

  return (
    <Box component="section" aria-labelledby="branding-fonts-heading" sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography id="branding-fonts-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.group.fonts', 'Self-hosted font faces')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {faces.length === 0 ? inheritedSummary : t('pages.admin.branding.font.replaces', 'These faces replace the ones the layer below declares.')}
      </Typography>
      {faces.map((face, index) => (
        <FaceRow
          // eslint-disable-next-line react/no-array-index-key
          key={index}
          index={index}
          face={face}
          disabled={disabled}
          uploading={uploading}
          onPatch={(change) => patch(index, change)}
          onRemove={() => onChange(faces.filter((_, at) => at !== index))}
          onFile={(file) => onUpload(file, { faceIndex: index })}
        />
      ))}
      {error === undefined ? null : (
        <FormHelperText error data-testid="branding-font-faces-error">
          {error}
        </FormHelperText>
      )}
      <Box>
        <Button
          size="small"
          variant="tertiary"
          startIcon={<AddIcon />}
          disabled={disabled || faces.length >= MAX_FONT_FACES}
          onClick={() => onChange([...faces, { family: '', url: '' }])}
          data-testid="branding-font-face-add"
        >
          {t('pages.admin.branding.font.add', 'Add face')}
        </Button>
      </Box>
    </Box>
  );
}

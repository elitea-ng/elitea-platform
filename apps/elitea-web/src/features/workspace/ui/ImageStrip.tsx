/**
 * The thumbnails of the images a message carries (#1229): in the composer
 * (with a remove button each) and above a sent prompt (read only, from the
 * thread history). A thumbnail is the small preview the host made; an image
 * it could not preview shows its name.
 */
import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/** What the strip shows of one image. */
export interface StripImage {
  name: string;
  thumbnail: string | null;
}

export interface ImageStripProps {
  images: readonly StripImage[];
  /** Absent: read only (a sent prompt). Called with the image's index. */
  onRemove?: ((index: number) => void) | undefined;
}

const THUMB = '3.5rem';

export function ImageStrip({ images, onRemove }: ImageStripProps): React.JSX.Element | null {
  if (images.length === 0) return null;
  return (
    <Box data-testid="image-strip" sx={{ display: 'flex', flexWrap: 'wrap', gap: 1 }}>
      {images.map((image, index) => (
        // oxlint-disable-next-line react/no-array-index-key -- a recorded turn's images carry no id; the strip is positional (removal goes by index).
        <Box
          key={`${image.name}:${String(index)}`}
          data-testid="image-thumb"
          sx={(theme: Theme) => ({
            position: 'relative',
            width: THUMB,
            height: THUMB,
            borderRadius: theme.vars.shape.radiusSm,
            border: `1px solid ${theme.vars.palette.divider}`,
            overflow: 'hidden',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          })}
        >
          {image.thumbnail === null ? (
            <Typography variant="bodySmall" sx={{ padding: 0.5, overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: '100%' }} title={image.name}>
              {image.name}
            </Typography>
          ) : (
            <Box component="img" src={image.thumbnail} alt={image.name} title={image.name} sx={{ width: '100%', height: '100%', objectFit: 'cover' }} />
          )}
          {onRemove !== undefined && (
            <IconButton
              size="small"
              aria-label={t('workspace.composer.removeImage', 'Remove {{name}}', { name: image.name })}
              onClick={() => onRemove(index)}
              sx={(theme: Theme) => ({
                position: 'absolute',
                top: 0,
                right: 0,
                padding: 0.25,
                background: theme.vars.palette.background.paper,
                '&:hover': { background: theme.vars.palette.background.paper },
              })}
            >
              <CloseIcon sx={{ fontSize: '0.875rem' }} />
            </IconButton>
          )}
        </Box>
      ))}
    </Box>
  );
}

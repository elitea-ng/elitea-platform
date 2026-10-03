/**
 * IconPickerDialog — the "Choose an icon" dialog: a Default section, an
 * Uploaded section with per-icon delete, and an upload button.
 *
 * WHY IT IS SHARED. It is the body of `ProjectIconDialog`, which is itself the
 * port of the old app's `SelectProjectIconDialog.jsx`. The old app had a SECOND
 * copy of the same dialog for skills (`components/SelectIconDialog.jsx`), and
 * porting that copy would have shipped the divergence along with it. Instead
 * the dialog is parameterised by its DATA — the two icon lists, the three
 * callbacks — and both `ProjectIconDialog` and `SkillIconDialog` are thin
 * wrappers that supply their own queries and mutations. A feature may not
 * import another feature (R-L1), so `shared/ui` is the only place both can
 * reach.
 *
 * Two defects the project dialog already paid for are preserved here rather
 * than re-introduced:
 *
 *  - the image branch has an error handler. A url that 404s falls back to the
 *    name's first letter; without it the grid draws broken-image boxes, which
 *    is what the default-icon catalogue produced when it served invented urls.
 *  - the theme is read by a `useTheme()` call INSIDE the component that uses
 *    it. The original read a constant defined in a different function
 *    component, so the identifier resolved to nothing at run time and the
 *    dialog threw as soon as it had one icon to draw.
 */
import { useCallback, useRef, useState } from 'react';
import type { ReactNode, RefObject } from 'react';

import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { DeletableIconTile } from './DeletableIconTile';
import { IconTile } from './IconTile';
import { typeScale } from '@/shared/brand/typography';

/** One icon in either section. `url` absent means "draw the letter fallback". */
export interface PickableIcon {
  readonly name: string;
  readonly url?: string | undefined;
}

/** The upload's measured (and capped) pixel box. */
export interface UploadDimensions {
  readonly width: number;
  readonly height: number;
}

export interface IconPickerDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  /** The icon currently worn by the entity; `null`/undefined means the default. */
  readonly selectedIcon?: PickableIcon | null | undefined;
  /** Name shown on the "no icon" tile — the entity's own name. */
  readonly placeholderName: string;
  readonly defaultIcons: readonly PickableIcon[];
  readonly loadingDefaultIcons?: boolean;
  readonly uploadedIcons: readonly PickableIcon[];
  readonly loadingUploadedIcons?: boolean;
  /** Called with the chosen icon's NAME, or `null` to reset to the default. */
  readonly onSelectIcon: (name: string | null) => void;
  readonly onUpload: (file: File, dimensions: UploadDimensions) => Promise<void>;
  readonly onDeleteIcon: (name: string) => Promise<void>;
}

/** The maximum icon edge the server stores; the client measures against it. */
const MAX_ICON_EDGE = 64;

const ACCEPTED_TYPES = '.jpg,.jpeg,.png,.tiff,.webp,.gif,.bmp,.ico';

/**
 * Decodes `file` through `Image()` to get its real (capped) pixel dimensions.
 * Never called for TIFF — browsers cannot decode TIFF via `Image()`, so
 * `image.onload` would never fire and the upload would hang forever.
 */
function readImageDimensions(file: File): Promise<UploadDimensions> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = (event) => {
      const image = new Image();
      image.onload = () => {
        resolve({
          width: Math.min(image.width, MAX_ICON_EDGE),
          height: Math.min(image.height, MAX_ICON_EDGE),
        });
      };
      image.onerror = () => { reject(new Error(`icon picker: failed to decode image "${file.name}"`)); };
      image.src = (event.target?.result as string | null) ?? '';
    };
    reader.onerror = () => { reject(new Error(`icon picker: failed to read file "${file.name}"`)); };
    reader.readAsDataURL(file);
  });
}

export function IconPickerDialog({
  open,
  onClose,
  selectedIcon,
  placeholderName,
  defaultIcons,
  loadingDefaultIcons = false,
  uploadedIcons,
  loadingUploadedIcons = false,
  onSelectIcon,
  onUpload,
  onDeleteIcon,
}: IconPickerDialogProps): ReactNode {
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [isUploading, setIsUploading] = useState(false);

  const handleSelectIcon = useCallback(
    (name: string | null) => {
      onSelectIcon(name);
      onClose();
    },
    [onSelectIcon, onClose],
  );

  const handleFileSelect = useCallback(
    (event: React.ChangeEvent<HTMLInputElement>) => {
      const file = event.target.files?.[0];
      if (!file) return;
      setIsUploading(true);
      const dimensions =
        file.type === 'image/tiff'
          ? Promise.resolve({ width: MAX_ICON_EDGE, height: MAX_ICON_EDGE })
          : readImageDimensions(file);
      void dimensions
        .then((measured) => onUpload(file, measured))
        .finally(() => {
          setIsUploading(false);
          if (fileInputRef.current) fileInputRef.current.value = '';
        })
        .catch(() => {
          // The mutation surfaces its own error; an unreadable file must not
          // leave the dialog stuck in its uploading state, which the finally
          // above already prevents.
        });
    },
    [onUpload],
  );

  const handleDeleteIcon = useCallback(
    async (name: string) => {
      try {
        await onDeleteIcon(name);
        // Reset the selection to the DEFAULT (never to the just-deleted name)
        // and only when the deleted icon was the selected one. The dialog
        // stays open either way — old-app parity.
        if (selectedIcon?.name === name) onSelectIcon(null);
      } catch {
        // Error surfaced by the mutation.
      }
    },
    [onDeleteIcon, onSelectIcon, selectedIcon],
  );

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('shared.iconPicker.title', 'Choose an icon')}
      content={
        <>
          <DefaultIconsSection
            placeholderName={placeholderName}
            selectedIcon={selectedIcon}
            defaultIcons={defaultIcons}
            loading={loadingDefaultIcons}
            onSelectIcon={handleSelectIcon}
          />
          <UploadedIconsSection
            uploadedIcons={uploadedIcons}
            loading={loadingUploadedIcons}
            selectedIcon={selectedIcon}
            onSelectIcon={handleSelectIcon}
            onDeleteIcon={handleDeleteIcon}
          />
          <input
            ref={fileInputRef}
            type="file"
            accept={ACCEPTED_TYPES}
            style={{ display: 'none' }}
            data-testid="icon-picker-file-input"
            onChange={handleFileSelect}
          />
        </>
      }
      actions={{
        node: (
          <UploadIconButton
            fileInputRef={fileInputRef}
            isUploading={isUploading}
          />
        ),
      }}
    />
  );
}

/* ── sections ──────────────────────────────────────────────────────────── */

function DefaultIconsSection({
  placeholderName,
  selectedIcon,
  defaultIcons,
  loading,
  onSelectIcon,
}: {
  readonly placeholderName: string;
  readonly selectedIcon?: PickableIcon | null | undefined;
  readonly defaultIcons: readonly PickableIcon[];
  readonly loading: boolean;
  readonly onSelectIcon: (name: string | null) => void;
}): ReactNode {
  return (
    <Box>
      <Typography
        variant="labelSmall"
        color="text.tertiary"
        sx={cx.sectionLabel}
      >
        {t('shared.iconPicker.defaultSection', 'Default')}
      </Typography>
      <Box sx={cx.iconGrid}>
        {!selectedIcon?.url && !selectedIcon?.name ? (
          <IconTile
            isSelected
            onClick={() => { onSelectIcon(null); }}
          >
            <IconPlaceholder name={placeholderName} />
          </IconTile>
        ) : null}
        {!loading &&
          defaultIcons.map((icon) => (
            <IconTile
              key={icon.name}
              isSelected={selectedIcon?.name === icon.name}
              onClick={() => { onSelectIcon(icon.name); }}
            >
              <IconPlaceholder
                name={icon.name}
                url={icon.url}
              />
            </IconTile>
          ))}
      </Box>
    </Box>
  );
}

function UploadedIconsSection({
  uploadedIcons,
  loading,
  selectedIcon,
  onSelectIcon,
  onDeleteIcon,
}: {
  readonly uploadedIcons: readonly PickableIcon[];
  readonly loading: boolean;
  readonly selectedIcon?: PickableIcon | null | undefined;
  readonly onSelectIcon: (name: string | null) => void;
  readonly onDeleteIcon: (name: string) => Promise<void>;
}): ReactNode {
  return (
    <Box>
      <Typography
        variant="labelSmall"
        color="text.tertiary"
        sx={cx.sectionLabel}
      >
        {t('shared.iconPicker.uploadedSection', 'Uploaded')}
      </Typography>
      <Box sx={cx.iconGrid}>
        {loading && (
          <Box sx={cx.loader}>
            <CircularProgress size={24} />
          </Box>
        )}
        {uploadedIcons.map((icon) => (
          <DeletableIconTile
            key={icon.name}
            isSelected={selectedIcon?.name === icon.name}
            onClick={() => { onSelectIcon(icon.name); }}
            onDelete={() => { void onDeleteIcon(icon.name); }}
          >
            <IconPlaceholder
              name={icon.name}
              url={icon.url}
            />
          </DeletableIconTile>
        ))}
        {!loading && uploadedIcons.length === 0 && (
          <Typography
            variant="bodySmall"
            color="text.tertiary"
          >
            {t('shared.iconPicker.noUploaded', 'No uploaded icons yet')}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

function UploadIconButton({
  fileInputRef,
  isUploading,
}: {
  readonly fileInputRef: RefObject<HTMLInputElement | null>;
  readonly isUploading: boolean;
}): ReactNode {
  return (
    <Box>
      <IconButton
        onClick={() => fileInputRef.current?.click()}
        disabled={isUploading}
        size="small"
        aria-label={t('shared.iconPicker.upload', 'Upload an icon')}
      >
        {isUploading ? <CircularProgress size={16} /> : <ArrowUpwardIcon />}
      </IconButton>
    </Box>
  );
}

/** Shows the icon image, or the first letter of its name when it has no url — or when that url fails. */
function IconPlaceholder({ name, url }: { readonly name: string; readonly url?: string | undefined }): ReactNode {
  const theme = useTheme();
  const [failedUrl, setFailedUrl] = useState<string | undefined>(undefined);
  const onError = useCallback(() => { setFailedUrl(url); }, [url]);

  if (url && failedUrl !== url) {
    return (
      <Box
        component="img"
        src={url}
        alt={name}
        onError={onError}
        sx={cx.iconImage}
      />
    );
  }
  return <Box sx={{ ...cx.fallbackIcon, ...typeScale(theme.typography.headingSmall) }}>{name ? name.charAt(0).toUpperCase() : '?'}</Box>;
}

const cx = {
  sectionLabel: { marginBottom: '0.5rem' },
  iconGrid: { display: 'flex', flexWrap: 'wrap', gap: '0.5rem' },
  loader: {
    display: 'flex',
    justifyContent: 'center',
    alignItems: 'center',
    width: '100%',
    padding: '1rem 0',
  },
  iconImage: {
    width: '2.25rem',
    height: '2.25rem',
    borderRadius: 'var(--el-shape-radiusPill, 9999px)',
    objectFit: 'cover',
  },
  fallbackIcon: {
    width: '2.25rem',
    height: '2.25rem',
    borderRadius: 'var(--el-shape-radiusPill, 9999px)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: 'text.primary',
  },
} as const;

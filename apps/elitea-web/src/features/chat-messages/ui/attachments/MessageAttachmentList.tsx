/**
 * ui/attachments/MessageAttachmentList.tsx — list of attachments rendered
 * inside a chat message, ported from
 * `apps/elitea-ui/src/components/Chat/MessageAttachmentList.jsx` (C4 batch).
 *
 * Splits attachments into images (displayed in a responsive grid) and
 * non-image files (displayed as compact cards).
 */
import { useCallback, useMemo, useState } from 'react';

import { Box, Typography } from '@mui/material';

import { NormalAttachment } from './NormalAttachment';
import { ViewImageAttachmentModal } from './ViewImageAttachmentModal';

import type { Attachment } from '@/entities/attachment/model/types';
import {
  getImageSource as getAttachmentImageSource,
  hasUnresolvedFilepath,
} from '@/entities/attachment/model/selectors';

/** Re-export for backwards compat — consumers importing from this file still get the type. */
export type { NormalAttachmentArtifactData } from './types';

export interface MessageAttachmentListProps {
  /** The list of attachment objects to render. */
  readonly items?: readonly Attachment[];
  /** Called when the user confirms removal — `fileName` is the display name, `fromStorage` whether to also delete from artifact storage. */
  readonly onRemoveAttachment?: (fileName: string, fromStorage: boolean) => void;
  /** Required to download an artifact-storage-backed attachment (forwarded to `NormalAttachment`/`ViewImageAttachmentModal`). */
  readonly projectId?: string;
  /** Called with a human-readable message on download failure or image load failure. */
  readonly onError?: (message: string) => void;
  /** Opens a text-like attachment in the canvas editor (issue #878) — forwarded to `NormalAttachment`. Omitted, no such control renders. */
  readonly onOpenFileInCanvas?: (source: { readonly bucket: string; readonly name: string }) => void;
}

/**
 * Renders a grouped list of attachments within a chat message.
 *
 * Matches the baseline `MessageAttachmentList.jsx` behaviour:
 * - Images are grouped and displayed in a responsive CSS grid (max ~16.25rem per column).
 * - Non-image files are displayed as compact cards (baseline: `NormalAttachment`).
 * - Returns `null` when there are no attachments.
 */
export function MessageAttachmentList({
  items = [],
  onRemoveAttachment,
  projectId,
  onError,
  onOpenFileInCanvas,
}: MessageAttachmentListProps): React.ReactElement | null {
  const { imagesItems, otherFilesItems } = useMemo(
    () =>
      items.reduce(
        (acc, file) => {
          if (isImageFile(file)) {
            acc.imagesItems.push(file);
          } else {
            acc.otherFilesItems.push(file);
          }
          return acc;
        },
        { imagesItems: [] as Attachment[], otherFilesItems: [] as Attachment[] },
      ),
    [items],
  );

  if (!items?.length) return null;

  return (
    <Box
      sx={{
        width: '100%',
        marginTop: '0.5rem',
        display: 'flex',
        flexDirection: 'column',
        gap: '0.5rem',
      }}
    >
      {imagesItems.length > 0 && (
        <Box
          sx={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, 16.25rem)',
            justifyContent: 'center',
            gap: '0.5rem',
            marginTop: '0.5rem',
          }}
        >
          {imagesItems.map((file, index) => (
            <ImageAttachmentCard
              key={getAttachmentKey(file, index)}
              attachment={file}
              onRemoveAttachment={onRemoveAttachment as (fileName: string, fromStorage: boolean) => void}
              {...(projectId !== undefined ? { projectId } : {})}
              {...(onError !== undefined ? { onError } : {})}
            />
          ))}
        </Box>
      )}

      {otherFilesItems.length > 0 && (
        <Box
          sx={{
            display: 'flex',
            gap: '0.5rem',
            width: '100%',
            flexDirection: 'row',
            flexWrap: 'wrap',
            marginTop: '0.5rem',
          }}
        >
          {otherFilesItems.map((file, index) => (
            <NormalAttachment
              key={getAttachmentKey(file, index)}
              attachment={file}
              onRemoveAttachment={onRemoveAttachment as (fileName: string, fromStorage: boolean) => void}
              {...(projectId !== undefined ? { projectId } : {})}
              {...(onError !== undefined ? { onError } : {})}
              {...(onOpenFileInCanvas !== undefined ? { onOpenFileInCanvas } : {})}
            />
          ))}
        </Box>
      )}
    </Box>
  );
}

/**
 * Renders a single image attachment card that opens the image preview modal
 * when clicked.
 *
 * When the attachment has no resolved image source yet (baseline:
 * `ImageAttachment.jsx`'s `isPending` — a `filepath:` URL not yet resolved
 * by the indexer), a text placeholder is shown instead of an `<img>` with an
 * empty `src`; when there is genuinely no source and no pending state, the
 * card renders nothing at all (baseline: `if (!imageSource && !isPending)
 * return null`).
 */
function ImageAttachmentCard({
  attachment,
  onRemoveAttachment,
  projectId,
  onError,
}: {
  readonly attachment: Attachment;
  readonly onRemoveAttachment?: (fileName: string, fromStorage: boolean) => void;
  readonly projectId?: string;
  readonly onError?: (message: string) => void;
}): React.ReactElement | null {
  const [isOpen, setIsOpen] = useState(false);
  const fileName = getAttachmentName(attachment);
  const imageSrc = getAttachmentImageSource(attachment);
  const isPending = imageSrc === null && hasUnresolvedFilepath(attachment);

  const handleImageError = useCallback(() => {
    /* eslint-disable-next-line i18next/no-literal-string — passed to caller's onError, not rendered directly */
    onError?.('Failed to load image');
  }, [onError]);

  if (imageSrc === null && !isPending) return null;

  return (
    <>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          position: 'relative',
          maxWidth: '16.25rem',
          maxHeight: '12rem',
          minHeight: '4rem',
          overflow: 'hidden',
          // eslint-disable-next-line elitea/ad-hoc-radius — image card border radius
          borderRadius: '0.5rem',
          cursor: imageSrc !== null ? 'pointer' : 'default',
          border: '1px solid',
          borderColor: 'divider',
        }}
        onClick={imageSrc !== null ? () => setIsOpen(true) : undefined}
        data-testid="chat-image-attachment"
        data-name={fileName}
      >
        {imageSrc !== null ? (
          <Box
            component="img"
            src={imageSrc}
            alt={fileName}
            sx={{
              maxWidth: '16.25rem',
              maxHeight: '12rem',
              objectFit: 'contain',
            }}
            onError={handleImageError}
          />
        ) : (
          <Typography
            variant="bodySmall"
            color="text.secondary"
            sx={{ textAlign: 'center', padding: '0.5rem', wordBreak: 'break-word' }}
          >
            {fileName}
          </Typography>
        )}
      </Box>
      {isOpen && (
        <ViewImageAttachmentModal
          open={true}
          onClose={() => setIsOpen(false)}
          attachment={attachment}
          onRemoveAttachment={onRemoveAttachment as (fileName: string, fromStorage: boolean) => void}
          {...(projectId !== undefined ? { projectId } : {})}
          {...(onError !== undefined ? { onError } : {})}
        />
      )}
    </>
  );
}

/**
 * Returns `true` when `attachment` is an image file.
 *
 * Ported from `apps/elitea-ui/src/utils/attachmentImageUtils.js:isAttachmentImage`
 * / `getAttachmentType` — the wire value the upload flow writes to
 * `item_details.attachment_type` is the bare literal `'image'` or
 * `'document'` (see `useUploadAttachments.js`'s `getAttachmentType`), never a
 * MIME type, so that's the primary signal here. `det.type` is kept as a
 * secondary fallback for a File-like `Attachment` whose `type` genuinely is
 * a MIME string (e.g. `'image/png'`).
 */
function isImageFile(attachment: Attachment): boolean {
  const det = attachment as Record<string, unknown>;
  const itemDet = det?.item_details as Record<string, unknown> | undefined;
  if (itemDet?.attachment_type === 'image') return true;
  const fallbackType = det?.type;
  return typeof fallbackType === 'string' && fallbackType.startsWith('image/');
}

/**
 * Returns the display name for an attachment.
 *
 * Ported from `entities/attachment/lib/attachment.helpers.js:getAttachmentName`.
 */
function getAttachmentName(attachment: Attachment): string {
  const det = attachment as Record<string, unknown>;
  const itemDet = det?.item_details as Record<string, unknown> | undefined;
  return (itemDet?.filepath as string) ||
    (itemDet?.name as string) ||
    (det?.name as string) ||
    '';
}

/** Returns a stable string key for an attachment list item. */
function getAttachmentKey(file: Attachment, index: number): string {
  const det = file as Record<string, unknown>;
  const uuid = det?.uuid as string | undefined;
  return uuid ?? `file_${index}`;
}

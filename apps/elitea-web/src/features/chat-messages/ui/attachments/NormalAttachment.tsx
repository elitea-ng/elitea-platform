/**
 * ui/attachments/NormalAttachment.tsx — file attachment card for chat messages
 * (non-image files), ported from
 * `apps/elitea-ui/src/components/Chat/NormalAttachment.jsx` (C4 batch).
 *
 * Renders a compact card for a normal (non-image) file attachment, with name,
 * download button, preview button (when enabled), and a remove button that
 * opens a confirmation modal before the caller-supplied `onRemoveAttachment`
 * fires.
 */
import { useCallback, useMemo, useState } from 'react';

import { Box, IconButton, Typography } from '@mui/material';

import Tooltip from '@mui/material/Tooltip';

import { downloadAttachmentFromArtifact, downloadAttachmentImage } from '@/entities/attachment';
import type { Attachment } from '@/entities/attachment/model/types';
import { getAttachmentName } from '@/entities/attachment/model/selectors';
import { getConfig } from '@/shared/config';

import { detectCanvasFileOpenKind } from '../../lib/canvasFileSource';

import { parseAttachmentFilepath, planAttachmentDownload } from './attachmentDownload.helpers';
import type { NormalAttachmentArtifactData } from './types';

const IconButtonAny = IconButton as React.ComponentType<
  React.ComponentProps<typeof IconButton> & { variant?: string }
>;

export interface NormalAttachmentProps {
  /** The attachment to render. */
  readonly attachment?: Attachment;
  /** Called when the user confirms removal — `fileName` is the display name, `fromStorage` whether to also delete from artifact storage. */
  readonly onRemoveAttachment?: (fileName: string, fromStorage: boolean) => void;
  /** Additional MUI sx overrides for the root card. */
  readonly sx?: Record<string, unknown>;
  /** When true, show a preview button in the action row. */
  readonly preview?: boolean;
  /** Called when the user taps the preview button — receives the artifact data shape. */
  readonly onOpenArtifactPreview?: (artifact: NormalAttachmentArtifactData) => void;
  /** Required to download an artifact-storage-backed attachment. */
  readonly projectId?: string;
  /** Called with a human-readable message on download failure. */
  readonly onError?: (message: string) => void;
  /** Opens this attachment in the canvas editor (issue #878) — offered only for a text-like, artifact-storage-backed attachment; omitted or ineligible, no such control renders. */
  readonly onOpenFileInCanvas?: (source: { readonly bucket: string; readonly name: string }) => void;
}

/**
 * Renders a normal (non-image) file attachment card inside a chat message.
 *
 * Matches the baseline `NormalAttachment.jsx` visual and interaction model:
 * compact card with the file name truncated with ellipsis, download and remove
 * actions on hover, optional preview, and a confirm-dialog before removal.
 */
export function NormalAttachment({
  attachment,
  onRemoveAttachment,
  sx = {},
  preview = false,
  onOpenArtifactPreview,
  projectId,
  onError,
  onOpenFileInCanvas,
}: NormalAttachmentProps): React.ReactElement | null {
  const attachmentName = getAttachmentName(attachment ?? {});
  // For old custom bucket attachments, use filepath (/{bucket}/{filename});
  // for new attachments, use name — baseline: NormalAttachment.jsx:39. Kept
  // separate from `attachmentName` (the display label): this is the RAW
  // identifier `onRemoveAttachment` expects, not a display string.
  const fileName = (() => {
    const det = (attachment ?? {}) as Record<string, unknown>;
    const itemDet = det?.item_details as Record<string, unknown> | undefined;
    return (itemDet?.filepath as string) ||
      (itemDet?.name as string) ||
      (det?.name as string) ||
      attachmentName;
  })();

  const [isHovering, setIsHovering] = useState(false);
  const [openAlert, setOpenAlert] = useState(false);
  const [needToRemoveFromStorage, setNeedToRemoveFromStorage] = useState(false);

  const onClickRemove = useCallback(
    (event: React.MouseEvent<HTMLElement>) => {
      event.stopPropagation();
      setOpenAlert(true);
    },
    [],
  );

  const onClickDownload = useCallback(
    (event: React.MouseEvent<HTMLElement>) => {
      event.stopPropagation();
      const plan = planAttachmentDownload(attachment ?? {});
      const reportError = (message: string): void => onError?.(message);

      if (plan.kind === 'legacy-base64') {
        downloadAttachmentImage(attachment ?? {}, reportError);
        return;
      }

      const config = getConfig();
      if (config.status !== 'ok' || projectId === undefined) {
        /* eslint-disable-next-line i18next/no-literal-string — passed to caller's onError, not rendered directly */
        reportError('Failed to download file from storage');
        return;
      }

      void downloadAttachmentFromArtifact(
        { baseUrl: config.config.vite_server_url, projectId, filepath: plan.filepath },
        reportError,
      );
    },
    [attachment, projectId, onError],
  );

  /**
   * Where this attachment lives in the artifact store, when it does, and
   * when canvas can meaningfully open it. Reuses the SAME download-routing
   * decision `onClickDownload` makes (`planAttachmentDownload`): a
   * `legacy-base64` attachment (an inline File, or a bucket the server marks
   * `__undefined__`) has no bucket/key pair for canvas to fetch, so it gets
   * no control at all rather than one that always fails.
   */
  const canvasSource = useMemo(() => {
    const plan = planAttachmentDownload(attachment ?? {});
    if (plan.kind !== 'artifact-storage') return undefined;
    const parsed = parseAttachmentFilepath(plan.filepath);
    if (parsed === null) return undefined;
    if (detectCanvasFileOpenKind(parsed.filename) === undefined) return undefined;
    return { bucket: parsed.bucket, name: parsed.filename };
  }, [attachment]);

  const onClickOpenInCanvas = useCallback(
    (event: React.MouseEvent<HTMLElement>) => {
      event.stopPropagation();
      if (canvasSource) onOpenFileInCanvas?.(canvasSource);
    },
    [canvasSource, onOpenFileInCanvas],
  );

  const onPreviewFile = useCallback(() => {
    const artifact = {
      filepath: '',
      id: attachmentName,
      isUploading: false,
      modified: new Date().toISOString(),
      name: attachmentName,
      type: ((attachment as Record<string, unknown>)?.item_details as Record<string, unknown>)?.attachment_type as string | undefined,
      bucket: ((attachment as Record<string, unknown>)?.item_details as Record<string, unknown>)?.bucket as string | undefined,
    } as unknown as NormalAttachmentArtifactData;
    onOpenArtifactPreview?.(artifact);
  }, [attachment, attachmentName, onOpenArtifactPreview]);

  const onCloseAlert = useCallback((event?: React.MouseEvent<HTMLElement>) => {
    event?.stopPropagation();
    setOpenAlert(false);
  }, []);

  const onConfirmDelete = useCallback(
    (event?: React.MouseEvent<HTMLElement>) => {
      event?.stopPropagation();
      onRemoveAttachment?.(fileName, needToRemoveFromStorage);
      setOpenAlert(false);
    },
    [fileName, needToRemoveFromStorage, onRemoveAttachment],
  );

  // Don't render if no valid attachment name (baseline: `!attachmentName` early return)
  if (!attachmentName) return null;

  return (
    <>
      <Box
        onMouseEnter={() => setIsHovering(true)}
        onMouseLeave={() => setIsHovering(false)}
        sx={(theme) => ({
          display: 'flex',
          width: '12.125rem',
          height: '2.25rem',
          // eslint-disable-next-line elitea/ad-hoc-radius — file card border radius
          borderRadius: '0.5rem',
          overflow: 'hidden',
          position: 'relative',
          gap: '0.75rem',
          padding: '0.375rem 0.75rem',
          alignItems: 'center',
          background: (theme as { palette?: { background?: { button?: { default?: string } } } })?.palette?.background?.button?.default,
          ...sx,
        })}
        data-testid="chat-artifact-file-card"
        data-name={attachmentName}
      >
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          sx={{
            flex: 1,
            minWidth: 0,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            lineHeight: 'normal',
          }}
        >
          {attachmentName}
        </Typography>
        <Box
          sx={{
            display: isHovering ? 'flex' : 'none',
            alignItems: 'center',
            justifyContent: 'flex-end',
            gap: '0.15rem',
          }}
        >
          {preview && (
            /* eslint-disable-next-line i18next/no-literal-string */
            <Tooltip title="View/Edit file" placement="top">
              <IconButtonAny variant="elitea" color="tertiary" size="small" onClick={onPreviewFile} aria-label="Preview attachment">
                👁
              </IconButtonAny>
            </Tooltip>
          )}
          {canvasSource && onOpenFileInCanvas && (
            /* eslint-disable-next-line i18next/no-literal-string */
            <Tooltip title="Open in canvas" placement="top">
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onClickOpenInCanvas}
                data-testid="attachment-open-in-canvas"
                aria-label="Open in canvas"
              >
                🖊
              </IconButtonAny>
            </Tooltip>
          )}
          <IconButtonAny variant="elitea" color="tertiary" size="small" onClick={onClickDownload} aria-label="Download attachment">
            ↓
          </IconButtonAny>
          <IconButtonAny variant="elitea" color="tertiary" size="small" onClick={onClickRemove} aria-label="Remove attachment">
            ✕
          </IconButtonAny>
        </Box>
      </Box>

      {/* Confirmation modal — baseline: Modal.DeleteEntityModal */}
      {openAlert && (
        <Box
          sx={{
            position: 'fixed',
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            // eslint-disable-next-line elitea/no-raw-color — overlay opacity
            backgroundColor: 'rgba(0,0,0,0.5)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 1300,
          }}
          onClick={onCloseAlert}
        >
          <Box
            sx={{
              backgroundColor: 'background.paper',
              // eslint-disable-next-line elitea/ad-hoc-radius — modal border radius
              borderRadius: '1rem',
              padding: '1.5rem',
              maxWidth: '34.5rem',
              width: '100%',
            }}
            onClick={(e: React.MouseEvent<HTMLElement>) => e.stopPropagation()}
          >
            {/* eslint-disable-next-line i18next/no-literal-string — confirmation dialog text */}
            <Typography variant="bodyMedium" sx={{ marginBottom: '1rem' }}>
              Are you sure you want to remove {fileName}?
            </Typography>
            <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'flex-start', marginTop: '-0.5rem' }}>
              <Box
                component="input"
                type="checkbox"
                checked={needToRemoveFromStorage}
                onChange={(e) => setNeedToRemoveFromStorage(e.target.checked)}
                sx={{ marginTop: '0.3125rem' }}
              />
              {/* eslint-disable-next-line i18next/no-literal-string — confirmation dialog text */}
              <Typography variant="bodyMedium" color="text.secondary">
                Also delete from attachment storage
              </Typography>
            </Box>
            <Box sx={{ display: 'flex', justifyContent: 'flex-end', gap: '0.5rem', marginTop: '1rem' }}>
              {/* eslint-disable-next-line i18next/no-literal-string — confirmation button */}
              <IconButtonAny size="small" onClick={onCloseAlert}>
                Cancel
              </IconButtonAny>
              {/* eslint-disable-next-line i18next/no-literal-string — confirmation button */}
              <IconButtonAny variant="elitea" color="primary" size="small" onClick={onConfirmDelete}>
                Remove
              </IconButtonAny>
            </Box>
          </Box>
        </Box>
      )}
    </>
  );
}
